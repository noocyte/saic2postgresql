package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrimOverlap(t *testing.T) {
	last := MatchedPoint{Latitude: 59.0, Longitude: 10.0}

	t.Run("drops duplicated prefix", func(t *testing.T) {
		matched := []MatchedPoint{
			{59.0 - 0.0002, 10.0}, // behind last (overlap region)
			{59.0 - 0.0001, 10.0},
			{59.0, 10.0}, // same as last
			{59.0 + 0.0001, 10.0},
			{59.0 + 0.0002, 10.0},
			{59.0 + 0.0003, 10.0},
		}
		got := trimOverlap(last, matched)
		if len(got) != 3 || got[0] != matched[3] {
			t.Fatalf("expected to keep last 3 points, got %v", got)
		}
	})

	t.Run("keeps everything when nothing is near", func(t *testing.T) {
		matched := []MatchedPoint{{60, 11}, {60.1, 11}, {60.2, 11}}
		got := trimOverlap(last, matched)
		if len(got) != len(matched) {
			t.Fatalf("expected unchanged slice, got %v", got)
		}
	})

	t.Run("only searches first half", func(t *testing.T) {
		matched := []MatchedPoint{{60, 11}, {60.1, 11}, {60.2, 11}, {59.0, 10.0}}
		got := trimOverlap(last, matched)
		if len(got) != len(matched) {
			t.Fatalf("expected unchanged slice, got %v", got)
		}
	})
}

func TestMatchInChunksTerminatesWhenLastChunkFails(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n > 50 {
			t.Errorf("too many OSRM calls, loop does not terminate")
		}
		coords := strings.Split(strings.TrimPrefix(r.URL.Path, "/match/v1/driving/"), ";")
		if len(coords) < 100 {
			// Short tail chunk: simulate OSRM NoMatch.
			w.Write([]byte(`{"code":"NoMatch"}`))
			return
		}
		var pts []string
		for _, c := range coords {
			pts = append(pts, "["+c+"]")
		}
		fmt.Fprintf(w, `{"code":"Ok","matchings":[{"confidence":1,"geometry":{"type":"LineString","coordinates":[%s]}}]}`,
			strings.Join(pts, ","))
	}))
	defer srv.Close()

	positions := make([]DrivePosition, 150)
	base := time.Unix(1_700_000_000, 0)
	for i := range positions {
		positions[i] = DrivePosition{
			Latitude:  59.0 + float64(i)*0.0001,
			Longitude: 10.0,
			Timestamp: base.Add(time.Duration(i) * time.Second),
		}
	}

	client := NewOSRMClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := client.MatchDrive(ctx, positions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 OSRM calls, got %d", calls.Load())
	}
	if len(got) != 100 {
		t.Fatalf("expected 100 matched points from first chunk, got %d", len(got))
	}
}
