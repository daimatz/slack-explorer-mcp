package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
)

// Error messages for user-facing errors
const (
	ErrSlackTokenNotConfigured = "Slack user token is not configured. Please set your Slack user token."
)

// SearchMessagesResponse represents the output for search_messages tool
type SearchMessagesResponse struct {
	WorkspaceURL string                 `json:"workspace_url"`
	Messages     *SearchMessagesMatches `json:"messages"`
}

type SearchMessagesMatches struct {
	Matches    []SearchMessage   `json:"matches"`
	Pagination *SearchPagination `json:"pagination,omitempty"`
}

type SearchMessage struct {
	User      string       `json:"user"`
	Text      string       `json:"text"`
	Timestamp string       `json:"ts"`
	Channel   *ChannelInfo `json:"channel,omitempty"`
	// Fill if the message is in a thread
	ThreadTs string `json:"thread_ts,omitempty"`
}

type ChannelInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	IsPrivate bool   `json:"is_private,omitempty"`
	IsMPIM    bool   `json:"is_mpim,omitempty"`
}

type SearchPagination struct {
	TotalCount int `json:"total_count"`
	Page       int `json:"page"`
	PageCount  int `json:"page_count"`
	PerPage    int `json:"per_page"`
	First      int `json:"first"`
	Last       int `json:"last"`
}

// Handler struct implements the MCP handler
type Handler struct {
	getClient      func(ctx context.Context) (SlackClient, error)
	userRepository *UserRepository
}

// NewHandler creates a new handler with Slack client
func NewHandler() *Handler {
	return &Handler{
		getClient: func(ctx context.Context) (SlackClient, error) {
			token, err := SlackUserTokenFromContext(ctx)
			if err != nil {
				return nil, fmt.Errorf("failed to get Slack token from context: %w", err)
			}
			return NewSlackClient(token), nil
		},
		userRepository: NewUserRepository(),
	}
}

// Close releases resources owned by Handler.
func (h *Handler) Close() {
	h.userRepository.Close()
}

// SearchMessages handles the search_messages tool call
func (h *Handler) SearchMessages(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	client, err := h.getClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(ErrSlackTokenNotConfigured), nil
	}

	// Get channel types from context (set via environment variable at startup)
	channelTypes := ChannelTypesFromContext(ctx)
	if err := h.validateChannelTypes(channelTypes); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate pagination compatibility with channel type filtering
	requestedPage := request.GetInt("page", 1)
	needsMultiPageFetch := len(channelTypes) > 0 && !h.isServerSideFilterable(channelTypes)
	if needsMultiPageFetch && requestedPage > 1 {
		return mcp.NewToolResultError("pagination (page > 1) is not supported when SLACK_CHANNEL_TYPES includes 'public', 'private', or multiple types. Please use page=1 and adjust 'count' parameter instead"), nil
	}

	query, params, err := h.buildSearchParams(buildSearchParamsRequest{
		Query:        request.GetString("query", ""),
		InChannel:    request.GetString("in_channel", ""),
		ChannelTypes: channelTypes,
		FromUser:     request.GetString("from_user", ""),
		With:         request.GetStringSlice("with", []string{}),
		Before:       request.GetString("before", ""),
		After:        request.GetString("after", ""),
		On:           request.GetString("on", ""),
		During:       request.GetString("during", ""),
		Has:          request.GetStringSlice("has", []string{}),
		HasMy:        request.GetStringSlice("hasmy", []string{}),
		Highlight:    request.GetBool("highlight", false),
		Sort:         request.GetString("sort", "score"),
		SortDir:      request.GetString("sort_dir", "desc"),
		Count:        request.GetInt("count", 20),
		Page:         request.GetInt("page", 1),
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var searchResult *slack.SearchMessages
	if needsMultiPageFetch {
		// Fetch multiple pages and filter until we have enough results
		searchResult, err = h.fetchAndFilterMultiplePages(client, query, params, channelTypes)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	} else {
		// Single page fetch (either no filtering or server-side filtering)
		searchResult, err = client.SearchMessages(query, params)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	response := h.convertToSearchResponse(searchResult)

	jsonData, err := json.Marshal(response)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}

	return mcp.NewToolResultText(string(jsonData)), nil
}

type buildSearchParamsRequest struct {
	Query        string
	InChannel    string
	ChannelTypes []string
	FromUser     string
	With         []string
	Before       string
	After        string
	On           string
	During       string
	Has          []string
	HasMy        []string
	Highlight    bool
	Sort         string
	SortDir      string
	Count        int
	Page         int
}

// buildSearchParams validates parameters, applies defaults, and builds search query and parameters
func (h *Handler) buildSearchParams(request buildSearchParamsRequest) (string, slack.SearchParameters, error) {
	var queryParts []string

	// Prevent modifiers in query field to enforce use of dedicated parameter fields
	if request.Query != "" {
		modifierPattern := regexp.MustCompile(`\b(from|in|before|after|on|during|has|is|with):`)
		if modifierPattern.MatchString(request.Query) {
			return "", slack.SearchParameters{}, fmt.Errorf("query field cannot contain modifiers (from:, in:, etc.). Please use the dedicated fields")
		}
		queryParts = append(queryParts, request.Query)
	}

	if request.InChannel != "" {
		queryParts = append(queryParts, fmt.Sprintf("in:%s", request.InChannel))
	}

	// Add channel type modifiers to query
	// Note: Slack only supports is:dm and is:mpim modifiers.
	// For 'public' and 'private', we need to use post-filtering which has pagination limitations.
	//
	// Strategy:
	// - If only 'dm' is specified: add is:dm to query (server-side filtering)
	// - If only 'mpim' is specified: add is:mpim to query (server-side filtering)
	// - Otherwise: use post-filtering (client-side, subject to pagination issues)
	if len(request.ChannelTypes) == 1 {
		switch request.ChannelTypes[0] {
		case "dm":
			queryParts = append(queryParts, "is:dm")
		case "mpim":
			queryParts = append(queryParts, "is:mpim")
			// For "public" or "private", no query modifier is available
		}
	}
	// For multiple types or public/private, we'll rely on post-filtering

	if request.FromUser != "" {
		if !strings.HasPrefix(request.FromUser, "U") {
			return "", slack.SearchParameters{}, fmt.Errorf("invalid user ID format. Must start with 'U' (e.g., 'U1234567')")
		}
		queryParts = append(queryParts, fmt.Sprintf("from:<@%s>", request.FromUser))
	}

	for _, with := range request.With {
		if with != "" {
			if !strings.HasPrefix(with, "U") {
				return "", slack.SearchParameters{}, fmt.Errorf("invalid user ID format in with parameter: '%s'. Must start with 'U' (e.g., 'U1234567')", with)
			}
			queryParts = append(queryParts, fmt.Sprintf("with:<@%s>", with))
		}
	}

	datePattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	if request.Before != "" {
		if !datePattern.MatchString(request.Before) {
			return "", slack.SearchParameters{}, fmt.Errorf("before date must be in YYYY-MM-DD format")
		}
		queryParts = append(queryParts, fmt.Sprintf("before:%s", request.Before))
	}
	if request.After != "" {
		if !datePattern.MatchString(request.After) {
			return "", slack.SearchParameters{}, fmt.Errorf("after date must be in YYYY-MM-DD format")
		}
		queryParts = append(queryParts, fmt.Sprintf("after:%s", request.After))
	}
	if request.On != "" {
		if !datePattern.MatchString(request.On) {
			return "", slack.SearchParameters{}, fmt.Errorf("on date must be in YYYY-MM-DD format")
		}
		queryParts = append(queryParts, fmt.Sprintf("on:%s", request.On))
	}
	if request.During != "" {
		queryParts = append(queryParts, fmt.Sprintf("during:%s", request.During))
	}

	for _, has := range request.Has {
		if has != "" {
			queryParts = append(queryParts, fmt.Sprintf("has:%s", has))
		}
	}

	for _, hasmy := range request.HasMy {
		if hasmy != "" {
			queryParts = append(queryParts, fmt.Sprintf("hasmy:%s", hasmy))
		}
	}

	if request.Count < 1 || request.Count > 100 {
		return "", slack.SearchParameters{}, fmt.Errorf("count must be between 1 and 100, got %d", request.Count)
	}
	if request.Page < 1 || request.Page > 100 {
		return "", slack.SearchParameters{}, fmt.Errorf("page must be between 1 and 100, got %d", request.Page)
	}
	if request.Sort != "score" && request.Sort != "timestamp" {
		return "", slack.SearchParameters{}, fmt.Errorf("sort must be 'score' or 'timestamp', got '%s'", request.Sort)
	}
	if request.SortDir != "asc" && request.SortDir != "desc" {
		return "", slack.SearchParameters{}, fmt.Errorf("sort_dir must be 'asc' or 'desc', got '%s'", request.SortDir)
	}

	searchQuery := strings.Join(queryParts, " ")

	params := slack.SearchParameters{
		Sort:          request.Sort,
		SortDirection: request.SortDir,
		Highlight:     request.Highlight,
		Count:         request.Count,
		Page:          request.Page,
	}

	return searchQuery, params, nil
}

// extractThreadTsFromPermalink extracts thread_ts from Slack permalink URL
func (h *Handler) extractThreadTsFromPermalink(permalink string) string {
	// Extract thread_ts from URL pattern like:
	// https://workspace.slack.com/archives/C123/p1234567890123456?thread_ts=1234567890.123456
	re := regexp.MustCompile(`[?&]thread_ts=([0-9.]+)`)
	matches := re.FindStringSubmatch(permalink)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// extractWorkspaceURLFromPermalink extracts workspace URL from Slack permalink
func (h *Handler) extractWorkspaceURLFromPermalink(permalink string) string {
	// Extract workspace URL from permalink pattern like:
	// https://workspace.slack.com/archives/C123/p1234567890123456
	// Returns: https://workspace.slack.com
	if permalink == "" {
		return ""
	}

	re := regexp.MustCompile(`^(https?://[^/]+\.slack\.com)`)
	matches := re.FindStringSubmatch(permalink)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// convertToSearchResponse converts Slack API response to our response format
func (h *Handler) convertToSearchResponse(result *slack.SearchMessages) *SearchMessagesResponse {
	response := &SearchMessagesResponse{
		WorkspaceURL: "",
		Messages: &SearchMessagesMatches{
			Matches: make([]SearchMessage, 0, len(result.Matches)),
		},
	}

	if len(result.Matches) > 0 {
		response.WorkspaceURL = h.extractWorkspaceURLFromPermalink(result.Matches[0].Permalink)
	}

	for _, match := range result.Matches {
		msg := SearchMessage{
			User:      match.User,
			Text:      match.Text,
			Timestamp: match.Timestamp,
			ThreadTs:  h.extractThreadTsFromPermalink(match.Permalink),
		}

		if match.Channel.ID != "" {
			channelType := h.detectChannelType(match.Channel)
			msg.Channel = &ChannelInfo{
				ID:        match.Channel.ID,
				Name:      match.Channel.Name,
				Type:      channelType,
				IsPrivate: match.Channel.IsPrivate,
				IsMPIM:    match.Channel.IsMPIM,
			}
		}

		response.Messages.Matches = append(response.Messages.Matches, msg)
	}

	response.Messages.Pagination = &SearchPagination{
		TotalCount: result.Paging.Total,
		Page:       result.Paging.Page,
		PageCount:  result.Paging.Pages,
		PerPage:    result.Paging.Count,
		First:      1,
		Last:       result.Paging.Pages,
	}

	return response
}

func (h *Handler) validateChannelTypes(channelTypes []string) error {
	validTypes := map[string]bool{
		"public":  true,
		"private": true,
		"dm":      true,
		"mpim":    true,
	}

	for _, ct := range channelTypes {
		if !validTypes[ct] {
			return fmt.Errorf("invalid channel_type: %s. Must be 'public', 'private', 'dm', or 'mpim'", ct)
		}
	}
	return nil
}

// fetchAndFilterMultiplePages fetches multiple pages from Slack and filters them
// until we have enough results or exhaust all pages
func (h *Handler) fetchAndFilterMultiplePages(
	client SlackClient,
	query string,
	params slack.SearchParameters,
	channelTypes []string,
) (*slack.SearchMessages, error) {
	var allMatches []slack.SearchMessage
	requestedCount := params.Count
	currentPage := params.Page
	var lastTotal int
	var maxPages int

	// Keep fetching pages until we have enough filtered results or run out of pages
	for {
		// Fetch current page
		pageParams := params
		pageParams.Page = currentPage

		result, err := client.SearchMessages(query, pageParams)
		if err != nil {
			return nil, err
		}

		maxPages = result.Paging.Pages
		lastTotal = result.Total

		// Filter matches from this page
		for _, match := range result.Matches {
			channelType := h.detectChannelType(match.Channel)
			if contains(channelTypes, channelType) {
				allMatches = append(allMatches, match)
			}
		}

		// Check if we have enough results or reached the end
		if len(allMatches) >= requestedCount || currentPage >= maxPages {
			break
		}

		// Move to next page
		currentPage++
	}

	// Trim to requested count
	if len(allMatches) > requestedCount {
		allMatches = allMatches[:requestedCount]
	}

	// Build final result
	finalResult := &slack.SearchMessages{
		Matches: allMatches,
		Paging: slack.Paging{
			Count: len(allMatches),
			Total: len(allMatches), // We don't know the true total across all pages
			Page:  params.Page,
			Pages: 1, // We've combined multiple pages into one result
		},
		Total: lastTotal, // Keep original total for reference
	}

	return finalResult, nil
}

func (h *Handler) filterByChannelTypes(
	result *slack.SearchMessages,
	channelTypes []string,
) *slack.SearchMessages {
	filtered := &slack.SearchMessages{
		Paging: result.Paging,
		Total:  result.Total,
	}

	for _, match := range result.Matches {
		channelType := h.detectChannelType(match.Channel)
		if contains(channelTypes, channelType) {
			filtered.Matches = append(filtered.Matches, match)
		}
	}

	// Note: We intentionally leave Paging.Count, Paging.Total, and Total unchanged
	// because they represent Slack's original pagination state across all pages.
	// Client-side filtering only sees the current page, so we cannot accurately
	// compute the true total count without fetching all pages.
	// The response may include fewer matches than indicated by these counters.

	return filtered
}

func (h *Handler) detectChannelType(channel slack.CtxChannel) string {
	// Check MPIM first, as MPIMs use G prefix but should be classified separately
	if channel.IsMPIM {
		return "mpim"
	}

	if strings.HasPrefix(channel.ID, "D") {
		return "dm"
	} else if strings.HasPrefix(channel.ID, "G") || (strings.HasPrefix(channel.ID, "C") && channel.IsPrivate) {
		return "private"
	} else if strings.HasPrefix(channel.ID, "C") && !channel.IsPrivate {
		return "public"
	}
	return ""
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// isServerSideFilterable checks if the channel types can be filtered by Slack API
// Returns true only if:
// - Exactly one type is specified, AND
// - That type is 'dm' or 'mpim' (which have Slack query modifiers)
func (h *Handler) isServerSideFilterable(channelTypes []string) bool {
	if len(channelTypes) != 1 {
		return false
	}
	return channelTypes[0] == "dm" || channelTypes[0] == "mpim"
}

// GetThreadRepliesResponse represents the response structure for get_thread_replies
type GetThreadRepliesResponse struct {
	Messages   []ThreadMessage `json:"messages"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type ThreadMessage struct {
	User       string     `json:"user"`
	Text       string     `json:"text"`
	Timestamp  string     `json:"ts"`
	ReplyCount int        `json:"reply_count,omitempty"`
	ReplyUsers []string   `json:"reply_users,omitempty"`
	Reactions  []Reaction `json:"reactions,omitempty"`
}

type Reaction struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Users []string `json:"users"`
}

// GetThreadReplies handles the get_thread_replies tool call
func (h *Handler) GetThreadReplies(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	client, err := h.getClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(ErrSlackTokenNotConfigured), nil
	}

	params, err := h.buildThreadRepliesParams(buildThreadRepliesRequest{
		ChannelID: request.GetString("channel_id", ""),
		ThreadTS:  request.GetString("thread_ts", ""),
		Limit:     request.GetInt("limit", 100),
		Cursor:    request.GetString("cursor", ""),
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	messages, hasMore, nextCursor, err := client.GetConversationReplies(params)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	response := h.convertToThreadResponse(messages, hasMore, nextCursor)

	jsonData, err := json.Marshal(response)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}

	return mcp.NewToolResultText(string(jsonData)), nil
}

type buildThreadRepliesRequest struct {
	ChannelID string
	ThreadTS  string
	Limit     int
	Cursor    string
}

func (h *Handler) buildThreadRepliesParams(request buildThreadRepliesRequest) (*slack.GetConversationRepliesParameters, error) {
	if request.ChannelID == "" {
		return nil, fmt.Errorf("channel_id is required")
	}
	if !strings.HasPrefix(request.ChannelID, "C") {
		return nil, fmt.Errorf("invalid channel ID format. Must start with 'C' (e.g., 'C1234567')")
	}

	if request.ThreadTS == "" {
		return nil, fmt.Errorf("thread_ts is required")
	}
	tsPattern := regexp.MustCompile(`^\d{10}\.\d{6}$`)
	if !tsPattern.MatchString(request.ThreadTS) {
		return nil, fmt.Errorf("thread_ts must be in format '1234567890.123456'")
	}

	if request.Limit < 1 || request.Limit > 1000 {
		return nil, fmt.Errorf("limit must be between 1 and 1000, got %d", request.Limit)
	}

	params := &slack.GetConversationRepliesParameters{
		ChannelID: request.ChannelID,
		Timestamp: request.ThreadTS,
		Limit:     request.Limit,
	}
	if request.Cursor != "" {
		params.Cursor = request.Cursor
	}

	return params, nil
}

func (h *Handler) convertToThreadResponse(messages []slack.Message, hasMore bool, nextCursor string) *GetThreadRepliesResponse {
	response := &GetThreadRepliesResponse{
		Messages: make([]ThreadMessage, 0, len(messages)),
		HasMore:  hasMore,
	}

	if nextCursor != "" {
		response.NextCursor = nextCursor
	}

	for _, msg := range messages {
		threadMsg := ThreadMessage{
			User:      msg.User,
			Text:      msg.Text,
			Timestamp: msg.Timestamp,
		}

		if msg.ReplyCount > 0 {
			threadMsg.ReplyCount = msg.ReplyCount
		}

		if len(msg.ReplyUsers) > 0 {
			threadMsg.ReplyUsers = msg.ReplyUsers
		}

		if len(msg.Reactions) > 0 {
			reactions := make([]Reaction, 0, len(msg.Reactions))
			for _, reaction := range msg.Reactions {
				reactions = append(reactions, Reaction{
					Name:  reaction.Name,
					Count: reaction.Count,
					Users: reaction.Users,
				})
			}
			threadMsg.Reactions = reactions
		}

		response.Messages = append(response.Messages, threadMsg)
	}

	return response
}

// UserProfile represents a user profile result
type UserProfile struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
	RealName    string `json:"real_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (h *Handler) GetUserProfiles(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	client, err := h.getClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(ErrSlackTokenNotConfigured), nil
	}

	userIDs := request.GetStringSlice("user_ids", []string{})

	if len(userIDs) == 0 {
		return mcp.NewToolResultError("user_ids is required and cannot be empty"), nil
	}
	if len(userIDs) > 100 {
		return mcp.NewToolResultError("user_ids cannot exceed 100 entries"), nil
	}

	var profiles []UserProfile

	for _, userID := range userIDs {
		profile := h.getUserProfile(client, userID)
		profiles = append(profiles, profile)
	}

	jsonData, err := json.Marshal(profiles)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}

	return mcp.NewToolResultText(string(jsonData)), nil
}

func (h *Handler) getUserProfile(client SlackClient, userID string) UserProfile {
	if !strings.HasPrefix(userID, "U") {
		return UserProfile{
			UserID: userID,
			Error:  "invalid user ID format. Must start with 'U' (e.g., 'U1234567')",
		}
	}

	slackProfile, err := client.GetUserProfile(userID)
	if err != nil {
		return UserProfile{
			UserID: userID,
			Error:  err.Error(),
		}
	}

	return UserProfile{
		UserID:      userID,
		DisplayName: slackProfile.DisplayName,
		RealName:    slackProfile.RealName,
		Email:       slackProfile.Email,
	}
}

// SearchUsersByName searches for users by display name
func (h *Handler) SearchUsersByName(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	client, err := h.getClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(ErrSlackTokenNotConfigured), nil
	}

	displayName := request.GetString("display_name", "")
	if displayName == "" {
		return mcp.NewToolResultError("display_name is required"), nil
	}
	exact := request.GetBool("exact", true)

	users, err := h.userRepository.FindByDisplayName(ctx, client, displayName, exact)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Convert slack.User to UserProfile
	var profiles []UserProfile
	for _, user := range users {
		profiles = append(profiles, UserProfile{
			UserID:      user.ID,
			DisplayName: user.Profile.DisplayName,
			RealName:    user.Profile.RealName,
			Email:       user.Profile.Email,
		})
	}

	jsonData, err := json.Marshal(profiles)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}

	return mcp.NewToolResultText(string(jsonData)), nil
}
