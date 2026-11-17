package boardview

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/requesttiming"
)

var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

func acceptedCodings(header string) (gzipped, identity bool) {
	quality := map[string]float64{}
	for part := range strings.SplitSeq(header, ",") {
		coding, parameters, _ := strings.Cut(strings.TrimSpace(part), ";")
		value := 1.0
		if name, raw, found := strings.Cut(strings.TrimSpace(parameters), "="); found && strings.EqualFold(strings.TrimSpace(name), "q") {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				continue
			}
			value = parsed
		}
		quality[strings.ToLower(strings.TrimSpace(coding))] = value
	}
	acceptable := func(coding string, fallback float64) bool {
		if value, found := quality[coding]; found {
			return value > 0
		}
		if value, found := quality["*"]; found {
			return value > 0
		}
		return fallback > 0
	}
	return acceptable("gzip", 0), acceptable("identity", 1)
}

func Respond(w http.ResponseWriter, r *http.Request, status int, value any) error {
	started := time.Now()
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		return fmt.Errorf("encode board response: %w", err)
	}
	header := w.Header()
	header.Add("Vary", "Accept-Encoding")
	gzipped, identity := acceptedCodings(strings.Join(r.Header.Values("Accept-Encoding"), ","))
	if !gzipped && !identity {
		w.WriteHeader(http.StatusNotAcceptable)
		return nil
	}
	header.Set("Content-Type", "application/json")
	payload := body.Bytes()
	if gzipped {
		var compressed bytes.Buffer
		writer := gzipWriters.Get().(*gzip.Writer)
		writer.Reset(&compressed)
		_, err := writer.Write(payload)
		if err == nil {
			err = writer.Close()
		}
		gzipWriters.Put(writer)
		if err != nil {
			return fmt.Errorf("compress board response: %w", err)
		}
		header.Set("Content-Encoding", "gzip")
		payload = compressed.Bytes()
	}
	header.Set("Content-Length", strconv.Itoa(len(payload)))
	if timing := requesttiming.From(r.Context()); timing != nil {
		timing.Encode.Add(int64(time.Since(started)))
	}
	w.WriteHeader(status)
	_, err := w.Write(payload)
	return err
}
