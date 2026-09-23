// Package mcpserver exposes the gateway's messaging capabilities as MCP tools.
//
// The server runs INSIDE the gateway process and calls the repositories and the
// session manager directly; there is no HTTP hop and no second credential. It
// is mounted by internal/httpapi on /mcp over the SDK's Streamable HTTP
// transport.
//
// Every tool derives its tenant from the authenticated principal in the request
// context, which internal/auth's middleware puts there. No tool accepts a
// tenant identifier as an argument: that would hand the client the ability to
// choose whose data it reads.
package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/auth"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

// version is reported to MCP clients.
const version = "0.1.0"

// Chat is the tool-facing view of a conversation.
type Chat struct {
	ChatJID         string `json:"chat_jid" jsonschema:"the WhatsApp JID of the conversation"`
	LastMessageAt   string `json:"last_message_at" jsonschema:"RFC 3339 timestamp of the most recent message"`
	LastMessageBody string `json:"last_message_body" jsonschema:"text of the most recent message"`
	LastDirection   string `json:"last_direction" jsonschema:"in if the last message was received, out if it was sent"`
	MessageCount    int    `json:"message_count" jsonschema:"how many messages are stored for this chat"`
}

// Message is the tool-facing view of a single message.
type Message struct {
	ID          string `json:"id" jsonschema:"the gateway's internal message id"`
	ChatJID     string `json:"chat_jid" jsonschema:"the conversation this message belongs to"`
	SenderJID   string `json:"sender_jid" jsonschema:"who sent the message"`
	WAMessageID string `json:"wa_message_id" jsonschema:"the WhatsApp message id"`
	Direction   string `json:"direction" jsonschema:"in for received, out for sent"`
	Body        string `json:"body" jsonschema:"the text content"`
	MediaType   string `json:"media_type,omitempty" jsonschema:"media type when the message carried an attachment"`
	Timestamp   string `json:"timestamp" jsonschema:"RFC 3339 timestamp of the message"`
}

// ListChatsInput are the arguments of list_chats.
type ListChatsInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum number of chats to return; the gateway applies its own default when omitted"`
}

// ListChatsOutput is the result of list_chats.
type ListChatsOutput struct {
	Chats []Chat `json:"chats" jsonschema:"conversations ordered by most recent activity first"`
}

// ListMessagesInput are the arguments of list_messages.
type ListMessagesInput struct {
	ChatJID string `json:"chat_jid" jsonschema:"the WhatsApp chat JID, for example 5215550001111@s.whatsapp.net"`
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum number of messages to return, newest first"`
}

// ListMessagesOutput is the result of list_messages.
type ListMessagesOutput struct {
	ChatJID  string    `json:"chat_jid" jsonschema:"the conversation that was read"`
	Messages []Message `json:"messages" jsonschema:"messages ordered newest first"`
}

// SendMessageInput are the arguments of send_message.
type SendMessageInput struct {
	To   string `json:"to" jsonschema:"destination WhatsApp JID or phone number in international format"`
	Body string `json:"body" jsonschema:"the text to send"`
}

// SendMessageOutput is the result of send_message.
type SendMessageOutput struct {
	WAMessageID string `json:"wa_message_id" jsonschema:"the id WhatsApp assigned to the sent message"`
	To          string `json:"to" jsonschema:"the destination the message was sent to"`
}

// FindContactInput are the arguments of find_contact.
type FindContactInput struct {
	Query string `json:"query" jsonschema:"a name, business name or phone number to look for; matching is case-insensitive and partial"`
}

// ContactCandidate is one match of find_contact.
type ContactCandidate struct {
	JID         string `json:"jid" jsonschema:"the WhatsApp JID to use with list_messages, sync_history or send_message"`
	Name        string `json:"name" jsonschema:"the best display name known for this contact"`
	HasMessages bool   `json:"has_messages" jsonschema:"true when messages for this chat are already stored and can be read straight away"`
}

// FindContactOutput is the result of find_contact.
type FindContactOutput struct {
	Contacts []ContactCandidate `json:"contacts" jsonschema:"matching contacts, ordered by name"`
}

// SyncHistoryInput are the arguments of sync_history.
type SyncHistoryInput struct {
	ChatJID string `json:"chat_jid" jsonschema:"the WhatsApp chat JID to backfill, for example 5215550001111@s.whatsapp.net"`
	Count   int    `json:"count,omitempty" jsonschema:"how many older messages to request; defaults to 50 and is capped at 200"`
}

// SyncHistoryOutput is the result of sync_history.
type SyncHistoryOutput struct {
	ChatJID         string `json:"chat_jid" jsonschema:"the conversation that was backfilled"`
	Inserted        int    `json:"inserted" jsonschema:"how many messages were new; zero means nothing older arrived"`
	OldestTimestamp string `json:"oldest_timestamp" jsonschema:"RFC 3339 timestamp of the oldest message now stored for this chat"`
	MoreAvailable   bool   `json:"more_available" jsonschema:"true when calling sync_history again is likely to bring more history"`
}

// Deps are what the tools need to do their work in-process.
type Deps struct {
	Messages store.Messages
	Sessions wa.SessionManager
	Logger   *slog.Logger
}

// New builds the MCP server and registers its tools.
//
// The returned server is safe to share across requests: it holds no
// per-caller state, because every tool resolves its tenant from the context of
// the request it is serving.
func New(deps Deps) *mcp.Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:        "whats-cloud-mcp",
		Title:       "WhatsApp Cloud Gateway",
		Description: "Read and send WhatsApp messages for the authenticated tenant.",
		Version:     version,
	}, nil)

	registerListChats(server, deps)
	registerListMessages(server, deps)
	registerSendMessage(server, deps)
	registerFindContact(server, deps)
	registerSyncHistory(server, deps)

	return server
}

func registerListChats(server *mcp.Server, deps Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_chats",
		Title:       "List chats",
		Description: "List the WhatsApp conversations of the authenticated tenant, most recent activity first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListChatsInput) (*mcp.CallToolResult, ListChatsOutput, error) {
		principal, errResult := requireScope(ctx, auth.ScopeMessagesRead)
		if errResult != nil {
			return errResult, ListChatsOutput{}, nil
		}

		chats, err := deps.Messages.ListChats(ctx, principal.TenantID, in.Limit)
		if err != nil {
			deps.Logger.ErrorContext(ctx, "mcp list_chats failed",
				slog.String("tenant_id", principal.TenantID), slog.String("error", err.Error()))
			return toolError("could not list chats"), ListChatsOutput{}, nil
		}

		out := ListChatsOutput{Chats: make([]Chat, 0, len(chats))}
		for _, c := range chats {
			out.Chats = append(out.Chats, Chat{
				ChatJID:         c.ChatJID,
				LastMessageAt:   formatTime(c.LastMessageAt),
				LastMessageBody: c.LastMessageBody,
				LastDirection:   string(c.LastDirection),
				MessageCount:    c.MessageCount,
			})
		}
		return nil, out, nil
	})
}

func registerListMessages(server *mcp.Server, deps Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_messages",
		Title:       "List messages",
		Description: "List the messages of one WhatsApp conversation, newest first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListMessagesInput) (*mcp.CallToolResult, ListMessagesOutput, error) {
		principal, errResult := requireScope(ctx, auth.ScopeMessagesRead)
		if errResult != nil {
			return errResult, ListMessagesOutput{}, nil
		}
		if in.ChatJID == "" {
			return toolError("chat_jid is required"), ListMessagesOutput{}, nil
		}

		messages, err := deps.Messages.ListByChat(ctx, principal.TenantID, in.ChatJID, in.Limit)
		if err != nil {
			deps.Logger.ErrorContext(ctx, "mcp list_messages failed",
				slog.String("tenant_id", principal.TenantID), slog.String("error", err.Error()))
			return toolError("could not list messages"), ListMessagesOutput{}, nil
		}

		out := ListMessagesOutput{ChatJID: in.ChatJID, Messages: make([]Message, 0, len(messages))}
		for _, m := range messages {
			msg := Message{
				ID:          m.ID,
				ChatJID:     m.ChatJID,
				SenderJID:   m.SenderJID,
				WAMessageID: m.WAMessageID,
				Direction:   string(m.Direction),
				Body:        m.Body,
				Timestamp:   formatTime(m.Timestamp),
			}
			if m.MediaType != nil {
				msg.MediaType = *m.MediaType
			}
			out.Messages = append(out.Messages, msg)
		}
		return nil, out, nil
	})
}

func registerSendMessage(server *mcp.Server, deps Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_message",
		Title:       "Send message",
		Description: "Send a WhatsApp text message as the authenticated tenant.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SendMessageInput) (*mcp.CallToolResult, SendMessageOutput, error) {
		principal, errResult := requireScope(ctx, auth.ScopeMessagesSend)
		if errResult != nil {
			return errResult, SendMessageOutput{}, nil
		}
		if in.To == "" {
			return toolError("to is required"), SendMessageOutput{}, nil
		}
		if in.Body == "" {
			return toolError("body is required"), SendMessageOutput{}, nil
		}

		waMessageID, err := deps.Sessions.SendText(ctx, principal.TenantID, in.To, in.Body)
		if err != nil {
			// The body is never logged.
			deps.Logger.ErrorContext(ctx, "mcp send_message failed",
				slog.String("tenant_id", principal.TenantID),
				slog.String("to", in.To),
				slog.String("error", err.Error()))
			return toolError(describeSendError(err)), SendMessageOutput{}, nil
		}

		if err := deps.Messages.Append(ctx, store.Message{
			ID:          store.NewID(),
			TenantID:    principal.TenantID,
			ChatJID:     in.To,
			WAMessageID: waMessageID,
			Direction:   store.DirectionOut,
			Body:        in.Body,
			Timestamp:   time.Now().UTC(),
			CreatedAt:   time.Now().UTC(),
		}); err != nil {
			// The message did leave; failing to record it is not a send failure.
			deps.Logger.ErrorContext(ctx, "could not persist outbound mcp message",
				slog.String("tenant_id", principal.TenantID),
				slog.String("wa_message_id", waMessageID),
				slog.String("error", err.Error()))
		}

		return nil, SendMessageOutput{WAMessageID: waMessageID, To: in.To}, nil
	})
}

// registerFindContact registers the entry point of the read flow.
//
// The description spells out the sequence because a model has no other way to
// learn it: a JID is not guessable from a name, and the gateway only stores what
// WhatsApp has already pushed to it.
func registerFindContact(server *mcp.Server, deps Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:  "find_contact",
		Title: "Find contact",
		Description: "Find the WhatsApp JID of a contact by name, business name or phone number. " +
			"This is the first step when the user names a person rather than a JID: search here, then read the conversation with list_messages. " +
			"Each candidate reports has_messages; when it is false, or the stored conversation turns out to be too short to answer the question, " +
			"call sync_history for that JID to pull older messages and then read it again.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in FindContactInput) (*mcp.CallToolResult, FindContactOutput, error) {
		principal, errResult := requireScope(ctx, auth.ScopeMessagesRead)
		if errResult != nil {
			return errResult, FindContactOutput{}, nil
		}
		if in.Query == "" {
			return toolError("query is required"), FindContactOutput{}, nil
		}

		contacts, err := deps.Sessions.FindContacts(ctx, principal.TenantID, in.Query)
		if err != nil {
			deps.Logger.ErrorContext(ctx, "mcp find_contact failed",
				slog.String("tenant_id", principal.TenantID), slog.String("error", err.Error()))
			return toolError(describeSyncError(err)), FindContactOutput{}, nil
		}

		out := FindContactOutput{Contacts: make([]ContactCandidate, 0, len(contacts))}
		for _, c := range contacts {
			out.Contacts = append(out.Contacts, ContactCandidate{
				JID:         c.JID,
				Name:        c.Name,
				HasMessages: c.HasMessages,
			})
		}
		return nil, out, nil
	})
}

// registerSyncHistory registers the backfill step.
//
// The anchor requirement is stated in the description on purpose: it is a
// constraint of WhatsApp's on-demand history rather than of this gateway, and a
// model that does not know it would keep retrying a call that can never work.
func registerSyncHistory(server *mcp.Server, deps Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:  "sync_history",
		Title: "Sync chat history",
		Description: "Ask the user's phone for older messages of one conversation, then store them. " +
			"Use it when list_messages returns nothing, or too little to answer the question, after find_contact gave you the chat JID; " +
			"call list_messages again afterwards to read what arrived. " +
			"The chat MUST already have at least one stored message to anchor the request on: WhatsApp only returns the messages immediately " +
			"before a message it can identify, so a conversation the gateway has never seen cannot be backfilled and must first appear in a " +
			"pushed history sync or receive a message. " +
			"The call waits for the phone to answer and may take a few seconds; repeat it to walk further back while more_available is true.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SyncHistoryInput) (*mcp.CallToolResult, SyncHistoryOutput, error) {
		principal, errResult := requireScope(ctx, auth.ScopeMessagesRead)
		if errResult != nil {
			return errResult, SyncHistoryOutput{}, nil
		}
		if in.ChatJID == "" {
			return toolError("chat_jid is required"), SyncHistoryOutput{}, nil
		}

		result, err := deps.Sessions.SyncHistory(ctx, principal.TenantID, in.ChatJID, wa.NormalizeSyncCount(in.Count))
		if err != nil {
			deps.Logger.ErrorContext(ctx, "mcp sync_history failed",
				slog.String("tenant_id", principal.TenantID),
				slog.String("chat_jid", in.ChatJID),
				slog.String("error", err.Error()))
			return toolError(describeSyncError(err)), SyncHistoryOutput{}, nil
		}

		return nil, SyncHistoryOutput{
			ChatJID:         result.ChatJID,
			Inserted:        result.Inserted,
			OldestTimestamp: formatTime(result.OldestTimestamp),
			MoreAvailable:   result.MoreAvailable,
		}, nil
	})
}

// requireScope enforces a tool's own scope requirement.
//
// The /mcp endpoint authenticates the tenant but cannot enforce a single scope,
// because the tools on it differ: reading needs messages:read and sending needs
// messages:send. So each tool checks for itself, and a missing scope produces a
// tool error rather than data.
func requireScope(ctx context.Context, required string) (auth.Principal, *mcp.CallToolResult) {
	principal, ok := auth.PrincipalFrom(ctx)
	if !ok || principal.TenantID == "" {
		return auth.Principal{}, toolError("not authenticated: a tenant api key is required")
	}
	if !principal.HasScope(required) {
		return auth.Principal{}, toolError("missing required scope: " + required)
	}
	return principal, nil
}

// toolError reports a failure to the model as tool output rather than as a
// protocol error, so the model can read the reason and react.
func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: message}},
	}
}

// describeSendError turns an engine failure into something a model can act on.
func describeSendError(err error) string {
	switch {
	case errors.Is(err, wa.ErrNotPaired):
		return "this tenant has no paired WhatsApp session; pair a number first"
	case errors.Is(err, wa.ErrInvalidJID):
		return "the destination is not a valid WhatsApp JID or phone number"
	case errors.Is(err, wa.ErrUnknownTenant):
		return "no WhatsApp session exists for this tenant"
	default:
		return "could not send the message"
	}
}

// describeSyncError turns a backfill failure into something a model can act on.
func describeSyncError(err error) string {
	switch {
	case errors.Is(err, wa.ErrNoAnchorMessage):
		return "this chat has no stored message to anchor a history request on; it must appear in a pushed history sync or receive a message before it can be backfilled"
	case errors.Is(err, wa.ErrSyncInProgress):
		return "a history sync for this chat is already running; wait for it to finish before asking again"
	case errors.Is(err, wa.ErrSyncTimeout):
		return "the phone did not answer the history request in time; it may still arrive, so read the chat again shortly"
	case errors.Is(err, wa.ErrNotPaired):
		return "this tenant has no paired WhatsApp session; pair a number first"
	case errors.Is(err, wa.ErrInvalidJID):
		return "the chat is not a valid WhatsApp JID"
	case errors.Is(err, wa.ErrUnknownTenant):
		return "no WhatsApp session exists for this tenant"
	default:
		return "could not sync the chat history"
	}
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }
