package notify

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"nvt/ver2/internal/jobs"
)

// Queue is what the watcher needs from the job queue.
type Queue interface {
	Subscribe() (<-chan jobs.Event, func())
	BatchView(id string) (jobs.BatchView, bool)
	CancelBatch(id string) int
}

// Watcher turns queue events into messages, and button presses back into
// actions on the queue.
type Watcher struct {
	q       Queue
	tg      *Telegram
	baseURL string

	mu       sync.Mutex
	messages map[string]int  // batch id -> the checkpoint message
	reported map[string]bool // batches already summarised
}

func NewWatcher(q Queue, tg *Telegram, baseURL string) *Watcher {
	return &Watcher{
		q: q, tg: tg, baseURL: strings.TrimRight(baseURL, "/"),
		messages: map[string]int{},
		reported: map[string]bool{},
	}
}

const stopPrefix = "stop:"

// Run listens until ctx is cancelled. It is safe to start even with no bot
// configured; it simply has nothing to say.
func (w *Watcher) Run(ctx context.Context) {
	if !w.tg.Enabled() {
		return
	}
	go w.tg.Poll(ctx, func(cb Callback) { w.onCallback(ctx, cb) })

	events, stop := w.q.Subscribe()
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Kind {
			case "checkpoint":
				w.onCheckpoint(ctx, ev)
			case "state":
				w.onState(ctx, ev)
			}
		}
	}
}

// onCheckpoint is the whole point: the first file is far enough along to look
// at, and everything else is still ahead. The batch keeps running — this is an
// offer to stop, not a pause.
func (w *Watcher) onCheckpoint(ctx context.Context, ev jobs.Event) {
	var b strings.Builder
	fmt.Fprintf(&b, "🎬 <b>첫 파일을 확인할 수 있습니다</b>\n%s\n\n", html.EscapeString(ev.Job.Name))
	fmt.Fprintf(&b, "진행 %.0f%%", ev.Job.Percent*100)
	if ev.Job.Rate > 0 {
		fmt.Fprintf(&b, " · %.2fx", ev.Job.Rate)
	}
	if ev.Job.ETASec > 0 {
		fmt.Fprintf(&b, " · 남은 시간 %s", human(ev.Job.ETASec))
	}
	b.WriteString("\n\n인트로가 지난 지점이라 자막이 화면에 나옵니다. ")
	b.WriteString("싱크와 화질이 괜찮은지 보고, 아니면 나머지를 멈추세요.")

	if bv, ok := w.q.BatchView(ev.BatchID); ok && bv.Pending > 1 {
		fmt.Fprintf(&b, "\n\n남은 파일 %d개", bv.Pending-1)
	}

	buttons := []Button{{Text: "■ 나머지 중단", Data: stopPrefix + ev.BatchID}}
	if url := w.watchURL(ev.Job.Rel); url != "" {
		buttons = append([]Button{{Text: "▶ 지금 보기", URL: url}}, buttons...)
	} else {
		b.WriteString("\n\n(웹 주소를 보려면 NVT2_PUBLIC_URL 을 설정하세요)")
	}

	id, err := w.tg.Send(ctx, b.String(), buttons)
	if err != nil {
		log.Printf("telegram checkpoint: %v", err)
		return
	}
	w.mu.Lock()
	w.messages[ev.BatchID] = id
	w.mu.Unlock()
}

// onState reports a batch once, when nothing in it is outstanding any more.
func (w *Watcher) onState(ctx context.Context, ev jobs.Event) {
	if !ev.Job.State.Terminal() {
		return
	}
	bv, ok := w.q.BatchView(ev.BatchID)
	if !ok || bv.Active {
		return
	}

	w.mu.Lock()
	if w.reported[ev.BatchID] {
		w.mu.Unlock()
		return
	}
	w.reported[ev.BatchID] = true
	msg := w.messages[ev.BatchID]
	delete(w.messages, ev.BatchID)
	w.mu.Unlock()

	var b strings.Builder
	switch {
	case bv.Failed > 0:
		fmt.Fprintf(&b, "⚠️ <b>변환 끝 — 실패 %d개</b>\n", bv.Failed)
	default:
		b.WriteString("✅ <b>변환이 끝났습니다</b>\n")
	}
	if bv.Dir != "" {
		fmt.Fprintf(&b, "%s\n", html.EscapeString(bv.Dir))
	}
	fmt.Fprintf(&b, "\n완료 %d개", bv.Done)
	if bv.Failed > 0 {
		fmt.Fprintf(&b, " · 실패 %d개", bv.Failed)
		for _, j := range bv.Jobs {
			if j.State == jobs.Failed {
				fmt.Fprintf(&b, "\n· %s — %s", html.EscapeString(j.Name), html.EscapeString(short(j.Error)))
			}
		}
	}

	if _, err := w.tg.Send(ctx, b.String(), nil); err != nil {
		log.Printf("telegram summary: %v", err)
	}
	// The stop button on the checkpoint message is meaningless now.
	if msg != 0 {
		w.tg.Edit(ctx, msg, "🎬 첫 파일 확인 안내 — 이 묶음은 끝났습니다.")
	}
}

func (w *Watcher) onCallback(ctx context.Context, cb Callback) {
	if !strings.HasPrefix(cb.Data, stopPrefix) {
		w.tg.Answer(ctx, cb.ID, "")
		return
	}
	batchID := strings.TrimPrefix(cb.Data, stopPrefix)
	n := w.q.CancelBatch(batchID)
	log.Printf("telegram stopped batch %s (%d job(s))", batchID, n)

	if n == 0 {
		w.tg.Answer(ctx, cb.ID, "이미 끝났거나 중단된 묶음입니다")
	} else {
		w.tg.Answer(ctx, cb.ID, fmt.Sprintf("%d개를 중단했습니다", n))
	}

	w.mu.Lock()
	msg := w.messages[batchID]
	delete(w.messages, batchID)
	w.mu.Unlock()
	if msg != 0 {
		w.tg.Edit(ctx, msg, fmt.Sprintf("■ 중단했습니다 — %d개 취소", n))
	}
}

// watchURL builds the link that goes in a notification.
//
// It has to be assembled with net/url rather than concatenated. Library paths
// hold spaces and Korean, and pasting a raw path into a URL leaves it to
// whoever handles the message next to guess at the encoding — Telegram
// escaped it a second time, turning a space into %2520, and the link then
// pointed at a file that does not exist.
func (w *Watcher) watchURL(rel string) string {
	if w.baseURL == "" || rel == "" {
		return ""
	}
	u, err := url.Parse(w.baseURL)
	if err != nil {
		return ""
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/watch/" + rel
	return u.String()
}

func human(sec float64) string {
	d := time.Duration(sec * float64(time.Second))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d초", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d분", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d시간 %d분", int(d.Hours()), int(d.Minutes())%60)
	}
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
