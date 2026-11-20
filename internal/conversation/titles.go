package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"code-agent/internal/agent"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/settings"
)

const MaxGeneratedTitleChars = 36

type turnTitleGenerator func(context.Context, string, string, string) (string, error)

func (g turnTitleGenerator) GenerateTitle(ctx context.Context, id, input, assistant string) (string, error) {
	return g(ctx, id, input, assistant)
}

func (e *TurnExecutor) titleGeneratorForTurn(mc settings.ModelConfig, explicit bool, cred credential.Resolver) TitleGenerator {
	if e.titleGen != nil {
		return e.titleGen
	}
	if builder := e.titleBuilder; builder != nil {
		return turnTitleGenerator(func(ctx context.Context, id, input, assistant string) (string, error) {
			gen, err := builder.BuildTitleGenerator(mc, explicit, cred)
			if err != nil {
				e.titles.report("[conversation-title] session_id=%q model=%q stage=provider reason=%T", id, mc.Model, err)
				return "", err
			}
			e.titles.report("[conversation-title] session_id=%q stage=generate outcome=start model=%q", id, mc.Model)
			return gen.GenerateTitle(ctx, id, input, assistant)
		})
	}
	return nil
}

// Titles coordinates asynchronous naming and publishes committed names.
type Titles struct {
	store    session.NameStore
	publish  func(agent.Event)
	logf     func(string, ...any)
	mu       sync.Mutex
	inFlight map[string]bool
}

func NewTitles(store session.NameStore, publish func(agent.Event)) *Titles {
	return &Titles{store: store, publish: publish, inFlight: make(map[string]bool)}
}

func (t *Titles) report(format string, args ...any) {
	if t != nil && t.logf != nil {
		t.logf(format, args...)
	}
}

func (t *Titles) Rename(ctx context.Context, id, name string) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.write(ctx, id, name, false)
}

func (t *Titles) write(ctx context.Context, id, name string, onlyIfEmpty bool) (int64, error) {
	ev := agent.Event{
		Kind: agent.EventConversationNameUpdated, SessionID: id, Name: name, At: time.Now().UTC(),
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	seq, changed, err := t.store.UpdateConversationName(ctx, id, name, onlyIfEmpty, session.EventRecord{
		SessionID: id, Kind: string(ev.Kind), At: ev.At, Payload: payload,
	})
	if changed && t.publish != nil {
		ev.Seq = seq
		t.publish(ev)
	}
	return seq, err
}

// Start takes a snapshot of the naming input before returning to the turn.
// accepted is used when the durable inbox has accepted a new message which
// has not been appended to the conversation history by the scheduler yet.
func (t *Titles) Start(ctx context.Context, gen TitleGenerator, sess *session.Session, accepted model.Message) {
	if t == nil || gen == nil || sess.Name != "" {
		return
	}
	input := titleInput(sess, accepted)
	if input == "" {
		return
	}
	id := sess.ID
	t.mu.Lock()
	if t.inFlight[id] {
		t.mu.Unlock()
		return
	}
	t.inFlight[id] = true
	t.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		defer func() {
			t.mu.Lock()
			delete(t.inFlight, id)
			t.mu.Unlock()
		}()
		if store, ok := t.store.(session.SessionHeaderStore); ok {
			saved, err := store.LoadHeader(ctx, id)
			if err != nil {
				t.report("[conversation-title] session_id=%q stage=load reason=%T", id, err)
				return
			}
			if saved.Name != "" {
				return
			}
		}
		title, err := gen.GenerateTitle(ctx, id, input, "")
		if err != nil {
			t.logGenerationFailure(id, err)
			return
		}
		title = strings.TrimSpace(title)
		if reason := generatedTitleRejection(title); reason != "" {
			t.report("[conversation-title] session_id=%q stage=validate reason=%s chars=%d", id, reason, utf8.RuneCountInString(title))
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		seq, err := t.write(ctx, id, title, true)
		if err != nil {
			t.report("[conversation-title] session_id=%q stage=save reason=%T", id, err)
		} else if seq > 0 {
			t.report("[conversation-title] session_id=%q stage=saved seq=%d", id, seq)
		}
	}()
}

func titleInput(sess *session.Session, accepted model.Message) string {
	for _, msg := range sess.Messages {
		if msg.Role == model.RoleUser && !msg.RuntimeInput {
			if input := titleMessageInput(msg); input != "" {
				return input
			}
		}
	}
	return titleMessageInput(accepted)
}

func titleMessageInput(msg model.Message) string {
	text := session.TitleInput(msg.Content)
	if text == "" {
		return ""
	}
	for _, asset := range msg.Assets {
		if asset.Kind == "image" || strings.HasPrefix(asset.MIMEType, "image/") {
			return text + "\nAttachment type: image"
		}
	}
	for _, asset := range msg.LocalAssets {
		if asset.Kind == "image" || strings.HasPrefix(asset.MIMEType, "image/") {
			return text + "\nAttachment type: image"
		}
	}
	return text
}

// StartAfterSave is the admission boundary for a local runner without a durable inbox.
func (t *Titles) StartAfterSave(ctx context.Context, gen TitleGenerator, sess *session.Session, save func(context.Context, *session.Session) error) {
	if gen == nil || sess.Name != "" {
		return
	}
	if store, ok := t.store.(session.SessionHeaderStore); ok {
		if saved, err := store.LoadHeader(ctx, sess.ID); err == nil && saved.Name != "" {
			sess.Name, sess.NameSeq = saved.Name, saved.NameSeq
			return
		}
	}
	if save(context.WithoutCancel(ctx), sess) == nil {
		t.Start(ctx, gen, sess, model.Message{})
	}
}

func (t *Titles) logGenerationFailure(id string, err error) {
	var apiErr *model.APIError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		t.report("[conversation-title] session_id=%q stage=generate reason=timeout", id)
	case errors.As(err, &apiErr):
		t.report("[conversation-title] session_id=%q stage=generate reason=api_error status=%d", id, apiErr.StatusCode)
	default:
		t.report("[conversation-title] session_id=%q stage=generate reason=%T", id, err)
	}
}

func generatedTitleRejection(title string) string {
	switch {
	case title == "":
		return "empty"
	case !utf8.ValidString(title):
		return "invalid_utf8"
	case utf8.RuneCountInString(title) > MaxGeneratedTitleChars:
		return "too_long"
	case strings.IndexFunc(title, func(r rune) bool {
		return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
	}) >= 0:
		return "control_character_or_multiline"
	default:
		return ""
	}
}
