# Slack Explorer MCP Server

A Model Context Protocol (MCP) server specialized in **retrieving information** from Slack messages and threads. It provides tools to access messages that the authenticated user can view using a User Token (xoxp).

## Available Tools

- Message Search (`search_messages`)
  - Search Slack messages with advanced filtering options. You can search by channel, user, date range, and specific features (reactions, files, etc.).
  - Parameters
    - `query`: Basic search query (without modifiers)
    - `in_channel`: Filter by channel name (e.g., "general", "team-dev")
    - `from_user`: Search messages from specific user (User ID)
    - `with`: Search DMs/threads with specific users (array of User IDs)
    - `before`, `after`, `on`: Date range filtering (YYYY-MM-DD format)
    - `during`: Period specification (e.g., "July", "2023")
    - `has`: Messages containing specific features (emoji, "pin", "file", "link", "reaction")
    - `hasmy`: Messages where you reacted with specific emoji
    - `sort`: Sort method ("score" or "timestamp")
    - `count`: Number of results per page (1-100, default: 20)
    - `page`: Page number (1-100, default: 1)

- Thread Replies (`get_thread_replies`)
  - Get all replies in a message thread. Supports pagination for efficiently handling large numbers of replies.
  - Parameters
    - `channel_id`: Channel ID (required)
    - `thread_ts`: Parent message timestamp (required)
    - `limit`: Number of replies to retrieve (1-1000, default: 100)
    - `cursor`: Pagination cursor

- User Profiles (`get_user_profiles`)
  - Get profile information for multiple users in bulk. Retrieve display names, real names, email addresses, and other profile information by specifying a list of user IDs.
  - Parameters
    - `user_ids`: Array of user IDs (required, max 100)

- Search Users by Display Name (`search_users_by_name`)
  - Search users by their display name. Supports both exact match and partial match search with case sensitivity.
  - Parameters
    - `display_name`: Display name to search for (required)
    - `exact`: Enable exact match search

## Setup

### Getting a Slack User Token

1. Create an app at [Slack API](https://api.slack.com/apps)
2. Add the following User Token Scopes in OAuth & Permissions:
   - `channels:history` - For public channels
   - `groups:history` - For private channels
   - `im:history` - For direct messages
   - `mpim:history` - For group direct messages
   - `search:read` - For message search
   - `users.profile:read` - For user profiles
   - `users:read` - For user information
3. Install the app to your workspace
4. Get the User OAuth Token (starts with xoxp-)
   - Tip: To use with multiple users in the same workspace, add them as Collaborators and have each user reinstall from OAuth & Permissions to get their own User OAuth Token

### MCP Server Configuration

1. Configure mcp.json

    ```json
    {
      "mcpServers": {
        "slack-explorer-mcp": {
          "command": "docker",
          "args": ["run", "-i", "--rm", "--pull", "always",
            "-e", "SLACK_USER_TOKEN=xoxp-your-token-here",
            "ghcr.io/shibayu36/slack-explorer-mcp:latest"
          ]
        }
      }
    }
    ```

    **Optional: Channel Type Filtering**

    To restrict search results to specific channel types (e.g., public channels only in shared MCP server environments), set the `SLACK_CHANNEL_TYPES` environment variable:

    ```json
    {
      "mcpServers": {
        "slack-explorer-mcp": {
          "command": "docker",
          "args": ["run", "-i", "--rm", "--pull", "always",
            "-e", "SLACK_USER_TOKEN=xoxp-your-token-here",
            "-e", "SLACK_CHANNEL_TYPES=public",
            "ghcr.io/shibayu36/slack-explorer-mcp:latest"
          ]
        }
      }
    }
    ```

    Available values (comma-separated for multiple):
    - `dm`: 1:1 direct messages only
    - `mpim`: Group DMs only
    - `public`: Public channels only
    - `private`: Private channels/groups only
    - If not specified: Search all types (default)

    **Filtering Strategy**:
    - When `dm` or `mpim` is specified **alone**: Uses Slack API search modifiers (`is:dm`, `is:mpim`) for efficient server-side filtering (supports pagination)
    - When `public`, `private`, or **multiple types** are specified: Automatically fetches multiple pages and filters results until the requested count is satisfied or all pages are exhausted

    **Important Limitations**:
    - When `public`, `private`, or multiple types are configured, **only `page=1` is supported**. Requests with `page > 1` will return an error
    - This is because client-side filtering cannot determine the filtered result count in advance, making accurate pagination impossible
    - Multi-page fetching may increase API call count as it retrieves results from multiple pages to satisfy the filter
    - Adjust the `count` parameter to retrieve more results in a single request

    Examples:
    - `SLACK_CHANNEL_TYPES=dm` (DMs only, server-side filtering)
    - `SLACK_CHANNEL_TYPES=public` (public channels only, multi-page fetching)
    - `SLACK_CHANNEL_TYPES=public,private` (channels only, excluding DMs, multi-page fetching)

    If you're using Claude Code:

    ```bash
    # Basic configuration
    claude mcp add slack-explorer-mcp -- docker run -i --rm --pull always \
      -e SLACK_USER_TOKEN=xoxp-your-token-here \
      ghcr.io/shibayu36/slack-explorer-mcp:latest

    # Restrict to public channels only
    claude mcp add slack-explorer-mcp -- docker run -i --rm --pull always \
      -e SLACK_USER_TOKEN=xoxp-your-token-here \
      -e SLACK_CHANNEL_TYPES=public \
      ghcr.io/shibayu36/slack-explorer-mcp:latest
    ```

2. Use the agent to perform Slack searches

    Examples:
    - "Search for meeting-related messages in the general channel from last week"
    - "Find messages from @john.doe about 'project'"
    - "Get all thread replies for this post"

## Usage

### Common Search Patterns

- **Search in a specific channel**
  ```
  Search for "release" messages in the general channel
  ```

- **Search messages from a specific user**
  ```
  Search for yesterday's messages from @john.doe
  ```

- **Search messages with reactions**
  ```
  Search for messages with :fire: reactions
  ```

- **Search messages you reacted to**
  ```
  Search for messages where you reacted with :eyes:
  ```

- **Search messages with file attachments**
  ```
  Search for messages with file attachments
  ```

### Using as Streamable HTTP Server

By default, the server uses stdio for MCP communication. You can start it as a Streamable HTTP server by setting the `TRANSPORT=http` environment variable. In HTTP mode, pass the Slack token using the `X-Slack-User-Token` header.

Starting the server:
```bash
# Start HTTP server (default: all interfaces 0.0.0.0, port 8080)
docker run -i --rm --pull always \
  -e TRANSPORT=http \
  -p 8080:8080 \
  ghcr.io/shibayu36/slack-explorer-mcp:latest

# Start with custom host and port
docker run -i --rm --pull always \
  -e TRANSPORT=http \
  -e HTTP_HOST=127.0.0.1 \
  -e HTTP_PORT=9090 \
  -p 9090:9090 \
  ghcr.io/shibayu36/slack-explorer-mcp:latest
```
