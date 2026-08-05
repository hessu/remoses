package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// maxBodyBytes caps every request body this package reads. CW text is the only
// field that can legitimately be large, and 64 KiB of Morse is over an hour of
// sending at any speed a human can copy.
const maxBodyBytes = 64 << 10

// decodeJSON reads a JSON request body into dst.
//
// Decoding is strict: an unrecognised field is an error rather than something
// quietly dropped. This API keys a transmitter, and a client that misspells
// "frequency" must be told so rather than be left believing that it tuned the
// radio.
//
// When required is false an entirely absent body is accepted, which is what
// POST /lock needs: its request body is optional.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, required bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) && !required {
			return nil
		}
		return fmt.Errorf("%w: %s", errUnprocessable, decodeMessage(err))
	}
	// A second document in the same body means the client is confused about
	// what it sent; accepting the first and ignoring the rest would hide it.
	if dec.More() {
		return fmt.Errorf("%w: unexpected content after the JSON document", errUnprocessable)
	}
	return nil
}

// decodeMessage renders a decode failure for the client. Everything it can
// report describes the client's own request body, so quoting it leaks nothing
// about the server.
func decodeMessage(err error) string {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return fmt.Sprintf("request body exceeds the %d byte limit", tooLarge.Limit)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "the request body is empty or truncated"
	}
	return err.Error()
}

// writeJSON writes a successful response.
//
// Cache-Control: no-store is set on everything: every response here embeds
// live radio state — frequency, PTT, lock holder — and a cached copy of any of
// it is worse than no copy at all.
func (s *server) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.log.Error("encoding response", "err", err)
		problem(w, http.StatusInternalServerError, "Internal Server Error",
			"the response could not be encoded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
