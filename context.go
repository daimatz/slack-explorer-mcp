package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// slackUserTokenKey is the context key for Slack user token
type slackUserTokenKey struct{}

// sessionIDKey is the context key for session ID
type sessionIDKey struct{}

// channelTypesKey is the context key for allowed channel types
type channelTypesKey struct{}

// SessionID represents a unique session identifier
type SessionID string

// SlackUserTokenFromContext retrieves Slack user token from context
func SlackUserTokenFromContext(ctx context.Context) (string, error) {
	token, ok := ctx.Value(slackUserTokenKey{}).(string)
	if !ok || token == "" {
		return "", fmt.Errorf("slack user token not found in context")
	}
	return token, nil
}

// WithSlackTokenFromEnv adds Slack token from environment variable to context
func WithSlackTokenFromEnv(ctx context.Context) context.Context {
	token := os.Getenv("SLACK_USER_TOKEN")
	if token == "" {
		// Return context as-is if no env var found
		return ctx
	}
	return withSlackUserToken(ctx, token)
}

// WithSlackTokenFromHTTP adds Slack token from HTTP header to context
func WithSlackTokenFromHTTP(ctx context.Context, r *http.Request) context.Context {
	token := r.Header.Get("X-Slack-User-Token")
	if token == "" {
		// Return context as-is if no token found in header
		return ctx
	}
	return withSlackUserToken(ctx, token)
}

func withSlackUserToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, slackUserTokenKey{}, token)
}

// WithSessionID adds session ID to context
func WithSessionID(ctx context.Context, sessionID SessionID) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext retrieves session ID from context
func SessionIDFromContext(ctx context.Context) SessionID {
	if sessionID, ok := ctx.Value(sessionIDKey{}).(SessionID); ok {
		return sessionID
	}
	return "default"
}

// WithChannelTypesFromEnv adds allowed channel types from environment variable to context
func WithChannelTypesFromEnv(ctx context.Context) context.Context {
	channelTypesStr := os.Getenv("SLACK_CHANNEL_TYPES")
	if channelTypesStr == "" {
		// If not set, allow all types
		return ctx
	}

	// Parse comma-separated list
	types := strings.Split(channelTypesStr, ",")
	var validTypes []string
	for _, t := range types {
		trimmed := strings.TrimSpace(t)
		if trimmed != "" {
			validTypes = append(validTypes, trimmed)
		}
	}

	if len(validTypes) == 0 {
		return ctx
	}

	return context.WithValue(ctx, channelTypesKey{}, validTypes)
}

// ChannelTypesFromContext retrieves allowed channel types from context
func ChannelTypesFromContext(ctx context.Context) []string {
	if types, ok := ctx.Value(channelTypesKey{}).([]string); ok {
		return types
	}
	// Return empty slice to indicate no filtering
	return []string{}
}
