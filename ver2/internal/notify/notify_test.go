package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nvt/ver2/internal/jobs"
)

// fakeAPI stands in for Telegram: it records what was sent and can hand back
// button presses.
type fakeAPI struct {
	*httptest.Server

	mu        sync.Mutex
	sent      []map[string]any
	edited    []map[string]any
	answered  []map[string]any
	callbacks []Callback
	nextMsgID int
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{nextMsgID: 100}
	mux := http.NewServeMux()

	record := func(into *[]map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			json.Unmarshal(body, &m)
			f.mu.Lock()
			*into = append(*into, m)
			f.nextMsgID++
			id := f.nextMsgID
			f.mu.Unlock()
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, id)
		}
	}
	mux.HandleFunc("/botTOKEN/sendMessage", record(&f.sent))
	mux.HandleFunc("/botTOKEN/editMessageText", record(&f.edited))
	mux.HandleFunc("/botTOKEN/answerCallbackQuery", record(&f.answered))

	mux.HandleFunc("/botTOKEN/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		pending := f.callbacks
		f.callbacks = nil
		f.mu.Unlock()

		if len(pending) == 0 {
			// Stand in for a long poll that timed out with nothing to report.
			time.Sleep(20 * time.Millisecond)
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
			return
		}
		var b strings.Builder
		b.WriteString(`{"ok":true,"result":[`)
		for i, cb := range pending {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"update_id":%d,"callback_query":{"id":%q,"data":%q}}`, i+1, cb.ID, cb.Data)
		}
		b.WriteString("]}")
		fmt.Fprint(w, b.String())
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) client() *Telegram {
	tg := NewTelegram("TOKEN", "12345")
	tg.api = f.URL
	return tg
}

func (f *fakeAPI) press(cb Callback) {
	f.mu.Lock()
	f.callbacks = append(f.callbacks, cb)
	f.mu.Unlock()
}

func (f *fakeAPI) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

// --- fake queue ---

type fakeQueue struct {
	events chan jobs.Event

	mu        sync.Mutex
	batch     jobs.BatchView
	cancelled []string
	cancelN   int
}

func (q *fakeQueue) Subscribe() (<-chan jobs.Event, func()) {
	return q.events, func() {}
}
func (q *fakeQueue) BatchView(id string) (jobs.BatchView, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.batch.ID != id {
		return jobs.BatchView{}, false
	}
	return q.batch, true
}
func (q *fakeQueue) CancelBatch(id string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cancelled = append(q.cancelled, id)
	return q.cancelN
}
func (q *fakeQueue) stopped() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.cancelled...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- tests ---

// No bot configured is the normal case for someone who has not set one up, and
// it must be silent rather than fatal.
func TestNilTelegramIsHarmless(t *testing.T) {
	var tg *Telegram
	if tg.Enabled() {
		t.Error("an unconfigured bot reported itself as enabled")
	}
	if id, err := tg.Send(context.Background(), "hello", nil); err != nil || id != 0 {
		t.Errorf("Send on a nil bot = %d, %v", id, err)
	}
	if err := tg.Edit(context.Background(), 1, "x"); err != nil {
		t.Errorf("Edit on a nil bot = %v", err)
	}
	tg.Poll(context.Background(), func(Callback) { t.Error("a nil bot polled") })

	if NewTelegram("", "chat") != nil || NewTelegram("token", "") != nil {
		t.Error("a half-configured bot should be nil")
	}
}

func TestSendIncludesButtons(t *testing.T) {
	api := newFakeAPI(t)
	id, err := api.client().Send(context.Background(), "hi", []Button{
		{Text: "watch", URL: "http://nas/watch/x"},
		{Text: "stop", Data: "stop:abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Error("no message id came back; editing later would be impossible")
	}

	msgs := api.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages", len(msgs))
	}
	raw, _ := json.Marshal(msgs[0]["reply_markup"])
	s := string(raw)
	if !strings.Contains(s, `"url":"http://nas/watch/x"`) {
		t.Errorf("link button missing: %s", s)
	}
	if !strings.Contains(s, `"callback_data":"stop:abc"`) {
		t.Errorf("stop button missing: %s", s)
	}
}

// The checkpoint is the reason this package exists: something to look at
// before the rest of the night is spent.
func TestCheckpointMessage(t *testing.T) {
	api := newFakeAPI(t)
	q := &fakeQueue{
		events: make(chan jobs.Event, 4),
		batch:  jobs.BatchView{ID: "b1", Dir: "애니/코난", Pending: 5, Active: true},
	}
	w := NewWatcher(q, api.client(), "http://nas:8081/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	q.events <- jobs.Event{Kind: "checkpoint", BatchID: "b1", Job: jobs.View{
		Name: "Conan - 2.mkv", Rel: "애니/코난/Conan - 2.mkv",
		Percent: 0.11, Rate: 0.72, ETASec: 2100, State: jobs.Running,
	}}

	waitFor(t, "the checkpoint message", func() bool { return len(api.messages()) == 1 })
	m := api.messages()[0]
	text, _ := m["text"].(string)

	for _, want := range []string{"Conan - 2.mkv", "11%", "0.72x", "35분", "남은 파일 4개"} {
		if !strings.Contains(text, want) {
			t.Errorf("checkpoint text is missing %q:\n%s", want, text)
		}
	}
	raw, _ := json.Marshal(m["reply_markup"])
	if !strings.Contains(string(raw), "stop:b1") {
		t.Errorf("no way to stop the batch: %s", raw)
	}
	if !strings.Contains(string(raw), "http://nas:8081/watch/") {
		t.Errorf("no link to look at the result: %s", raw)
	}
}

// Without a public URL there is no link to give, and saying so is better than
// sending a button that goes nowhere.
func TestCheckpointWithoutAPublicURL(t *testing.T) {
	api := newFakeAPI(t)
	q := &fakeQueue{events: make(chan jobs.Event, 4), batch: jobs.BatchView{ID: "b1", Active: true}}
	w := NewWatcher(q, api.client(), "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	q.events <- jobs.Event{Kind: "checkpoint", BatchID: "b1", Job: jobs.View{Name: "a.mkv", Rel: "a.mkv"}}
	waitFor(t, "the message", func() bool { return len(api.messages()) == 1 })

	m := api.messages()[0]
	raw, _ := json.Marshal(m["reply_markup"])
	if strings.Contains(string(raw), `"url"`) {
		t.Errorf("a link button was sent with no base url: %s", raw)
	}
	if !strings.Contains(m["text"].(string), "NVT2_PUBLIC_URL") {
		t.Error("the message does not explain why there is no link")
	}
}

// Pressing stop from the phone has to reach the queue — that is the whole
// point of polling instead of waiting for a webhook.
func TestStopButtonCancelsTheBatch(t *testing.T) {
	api := newFakeAPI(t)
	q := &fakeQueue{events: make(chan jobs.Event, 4), cancelN: 4,
		batch: jobs.BatchView{ID: "b1", Active: true}}
	w := NewWatcher(q, api.client(), "http://nas")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	api.press(Callback{ID: "cb1", Data: "stop:b1"})
	waitFor(t, "the batch to be cancelled", func() bool { return len(q.stopped()) == 1 })

	if got := q.stopped()[0]; got != "b1" {
		t.Errorf("cancelled %q, want b1", got)
	}
	waitFor(t, "the press to be acknowledged", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.answered) == 1
	})
	api.mu.Lock()
	defer api.mu.Unlock()
	if txt, _ := api.answered[0]["text"].(string); !strings.Contains(txt, "4개") {
		t.Errorf("acknowledgement = %q, want it to say how many stopped", txt)
	}
}

func TestUnknownCallbackIsAcknowledgedAndIgnored(t *testing.T) {
	api := newFakeAPI(t)
	q := &fakeQueue{events: make(chan jobs.Event, 4)}
	w := NewWatcher(q, api.client(), "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	api.press(Callback{ID: "cb1", Data: "something-else"})
	waitFor(t, "the acknowledgement", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.answered) == 1
	})
	if got := q.stopped(); len(got) != 0 {
		t.Errorf("an unrelated button cancelled %v", got)
	}
}

func TestBatchSummarySentOnceWhenEverythingIsDone(t *testing.T) {
	api := newFakeAPI(t)
	q := &fakeQueue{events: make(chan jobs.Event, 8), batch: jobs.BatchView{
		ID: "b1", Dir: "애니/코난", Done: 3, Failed: 0, Active: false,
	}}
	w := NewWatcher(q, api.client(), "http://nas")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Every finishing job produces a state event; only one summary may result.
	for i := 0; i < 3; i++ {
		q.events <- jobs.Event{Kind: "state", BatchID: "b1", Job: jobs.View{
			Name: fmt.Sprintf("ep%d.mkv", i), State: jobs.Done,
		}}
	}
	waitFor(t, "the summary", func() bool { return len(api.messages()) == 1 })
	time.Sleep(100 * time.Millisecond)

	msgs := api.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d summaries, want exactly 1", len(msgs))
	}
	text := msgs[0]["text"].(string)
	if !strings.Contains(text, "완료 3개") || !strings.Contains(text, "애니/코난") {
		t.Errorf("summary = %q", text)
	}
}

func TestBatchSummaryNamesFailures(t *testing.T) {
	api := newFakeAPI(t)
	q := &fakeQueue{events: make(chan jobs.Event, 4), batch: jobs.BatchView{
		ID: "b1", Done: 1, Failed: 1, Active: false,
		Jobs: []jobs.View{
			{Name: "ok.mkv", State: jobs.Done},
			{Name: "bad.mkv", State: jobs.Failed, Error: "no video stream"},
		},
	}}
	w := NewWatcher(q, api.client(), "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	q.events <- jobs.Event{Kind: "state", BatchID: "b1", Job: jobs.View{State: jobs.Failed}}
	waitFor(t, "the summary", func() bool { return len(api.messages()) == 1 })

	text := api.messages()[0]["text"].(string)
	for _, want := range []string{"실패", "bad.mkv", "no video stream"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary is missing %q:\n%s", want, text)
		}
	}
}

// A batch that is still running must not be summarised early.
func TestNoSummaryWhileTheBatchIsActive(t *testing.T) {
	api := newFakeAPI(t)
	q := &fakeQueue{events: make(chan jobs.Event, 4), batch: jobs.BatchView{
		ID: "b1", Done: 1, Pending: 2, Active: true,
	}}
	w := NewWatcher(q, api.client(), "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	q.events <- jobs.Event{Kind: "state", BatchID: "b1", Job: jobs.View{State: jobs.Done}}
	time.Sleep(150 * time.Millisecond)
	if n := len(api.messages()); n != 0 {
		t.Errorf("sent %d messages for a batch that is still running", n)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[float64]string{30: "30초", 150: "2분", 2100: "35분", 4000: "1시간 6분"}
	for sec, want := range cases {
		if got := human(sec); got != want {
			t.Errorf("human(%v) = %q, want %q", sec, got, want)
		}
	}
}
