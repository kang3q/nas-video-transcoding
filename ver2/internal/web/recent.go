package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"

	"nvt/ver2/internal/history"
)

// Picking up where you stopped needs two small things: the player saying
// where it got to, and the page knowing that before it starts.
//
// The position is reported by the browser even while the picture is on a
// television — AirPlay makes the page the remote control, and the video
// element goes on counting. So an episode half watched on the big screen is
// still half watched when the laptop is opened again.

type recentRow struct {
	history.Entry
	Href    string
	Percent float64
	Missing bool // the file has gone since it was watched
}

type recentData struct {
	Rows []recentRow
}

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	var data recentData
	for _, e := range s.history.Recent(100) {
		rel, err := s.mapper.ParseRel(e.Rel)
		if err != nil {
			continue
		}
		row := recentRow{
			Entry: e,
			Href:  (&url.URL{Path: "/watch/" + e.Rel}).String(),
		}
		if e.Duration > 0 {
			row.Percent = e.Pos / e.Duration
		}
		// Something watched and then deleted is still worth listing, with a
		// word about why the link leads nowhere.
		if _, err := os.Stat(s.mapper.Source(rel)); err != nil {
			if _, err := os.Stat(s.mapper.Output(rel)); err != nil {
				row.Missing = true
			}
		}
		data.Rows = append(data.Rows, row)
	}
	s.render(w, "recent", "마지막 시청", "recent", data)
}

// handleProgress takes a position from the player. It is called every few
// seconds and on the way out of the page, so it answers with nothing and
// costs nothing.
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Rel      string  `json:"rel"`
		Pos      float64 `json:"pos"`
		Duration float64 `json:"duration"`
	}
	// A beacon sent as the page closes has no content type worth trusting
	// and a body small enough to read whole.
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || json.Unmarshal(body, &in) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	rel, err := s.mapper.ParseRel(in.Rel)
	if err != nil || rel.IsRoot() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.history.Note(rel.String(), rel.Base(), in.Pos, in.Duration)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, http.StatusBadRequest, "요청을 읽을 수 없습니다")
		return
	}
	rel, err := s.mapper.ParseRel(r.FormValue("rel"))
	if err != nil || rel.IsRoot() {
		s.fail(w, http.StatusBadRequest, "잘못된 경로입니다")
		return
	}
	s.history.Forget(rel.String())
	http.Redirect(w, r, "/recent/", http.StatusSeeOther)
}
