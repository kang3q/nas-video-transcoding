// Package notify tells you when the first file of a batch is far enough along
// to judge, and lets you stop the rest from wherever you are.
//
// Conversion here runs at about 0.7x realtime, so a folder of episodes is a
// night's work or more. If the subtitles are out of sync or the wrong track
// was burned in, finding out at the end is finding out too late. The first
// file crossing ten percent is the earliest moment there is something real to
// look at — past an opening sequence, with subtitles actually on screen.
//
// The bot polls Telegram rather than receiving webhooks, so the stop button
// works from outside the house with no port forwarding and nothing exposed.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Button struct {
	Text string
	// URL opens a link. Only one of URL and Data is used.
	URL string
	// Data comes back as a callback when the button is pressed.
	Data string
}

type Telegram struct {
	token  string
	chat   string
	client *http.Client
	api    string // overridable for tests
}

// NewTelegram returns nil when no token is configured, and every method on a
// nil receiver is a no-op — notifications are optional, not a dependency.
func NewTelegram(token, chat string) *Telegram {
	if token == "" || chat == "" {
		return nil
	}
	return &Telegram{
		token:  token,
		chat:   chat,
		client: &http.Client{Timeout: 40 * time.Second},
		api:    "https://api.telegram.org",
	}
}

func (t *Telegram) Enabled() bool { return t != nil }

func (t *Telegram) method(name string) string {
	return fmt.Sprintf("%s/bot%s/%s", t.api, t.token, name)
}

type sendResult struct {
	OK     bool `json:"ok"`
	Result struct {
		MessageID int `json:"message_id"`
	} `json:"result"`
	Description string `json:"description"`
}

// Send posts a message, optionally with buttons under it. The returned id can
// be used to edit the message later, which is how a used-up button is retired.
func (t *Telegram) Send(ctx context.Context, text string, buttons []Button) (int, error) {
	if t == nil {
		return 0, nil
	}
	payload := map[string]any{
		"chat_id":                  t.chat,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if kb := keyboard(buttons); kb != nil {
		payload["reply_markup"] = kb
	}

	var res sendResult
	if err := t.call(ctx, "sendMessage", payload, &res); err != nil {
		return 0, err
	}
	return res.Result.MessageID, nil
}

// Edit replaces a message's text and removes its buttons. Used once a batch is
// stopped or finished, so a stale message cannot be acted on again.
func (t *Telegram) Edit(ctx context.Context, messageID int, text string) error {
	if t == nil || messageID == 0 {
		return nil
	}
	return t.call(ctx, "editMessageText", map[string]any{
		"chat_id":                  t.chat,
		"message_id":               messageID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}, nil)
}

// Answer acknowledges a button press. Telegram shows a spinner on the button
// until this arrives.
func (t *Telegram) Answer(ctx context.Context, callbackID, text string) error {
	if t == nil {
		return nil
	}
	return t.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	}, nil)
}

func keyboard(buttons []Button) map[string]any {
	if len(buttons) == 0 {
		return nil
	}
	row := make([]map[string]any, 0, len(buttons))
	for _, b := range buttons {
		btn := map[string]any{"text": b.Text}
		if b.URL != "" {
			btn["url"] = b.URL
		} else {
			btn["callback_data"] = b.Data
		}
		row = append(row, btn)
	}
	return map[string]any{"inline_keyboard": [][]map[string]any{row}}
}

func (t *Telegram) call(ctx context.Context, method string, payload map[string]any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.method(method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram %s: %s: %s", method, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	var probe struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if json.Unmarshal(raw, &probe) == nil && !probe.OK {
		return fmt.Errorf("telegram %s: %s", method, probe.Description)
	}
	return nil
}

// --- receiving ---

type Callback struct {
	ID   string
	Data string
}

type updates struct {
	OK     bool `json:"ok"`
	Result []struct {
		UpdateID      int `json:"update_id"`
		CallbackQuery *struct {
			ID   string `json:"id"`
			Data string `json:"data"`
		} `json:"callback_query"`
	} `json:"result"`
}

// Poll long-polls for button presses until ctx is cancelled.
//
// Long polling rather than a webhook is what makes this work from a NAS: no
// inbound connection, no port forwarding, nothing published.
func (t *Telegram) Poll(ctx context.Context, fn func(Callback)) {
	if t == nil {
		return
	}
	var offset int
	for {
		if ctx.Err() != nil {
			return
		}
		got, next, err := t.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A network blip must not end the loop; back off and carry on.
			log.Printf("telegram poll: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
			}
			continue
		}
		offset = next
		for _, cb := range got {
			fn(cb)
		}
	}
}

func (t *Telegram) getUpdates(ctx context.Context, offset int) ([]Callback, int, error) {
	q := url.Values{
		"timeout":         {"30"},
		"allowed_updates": {`["callback_query"]`},
	}
	if offset > 0 {
		q.Set("offset", fmt.Sprint(offset))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.method("getUpdates")+"?"+q.Encode(), nil)
	if err != nil {
		return nil, offset, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, offset, err
	}
	defer resp.Body.Close()

	var u updates
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&u); err != nil {
		return nil, offset, err
	}

	var out []Callback
	next := offset
	for _, r := range u.Result {
		if r.UpdateID >= next {
			next = r.UpdateID + 1
		}
		if r.CallbackQuery != nil {
			out = append(out, Callback{ID: r.CallbackQuery.ID, Data: r.CallbackQuery.Data})
		}
	}
	return out, next, nil
}
