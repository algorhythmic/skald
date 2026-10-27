package daemon

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/algorhythmic/skald/internal/retrieval"
)

func (a *API) extendedRead(w http.ResponseWriter, r *http.Request, ns []string) bool {
	path := r.URL.Path
	if path != "/v1/descriptions" && path != "/v1/context" && path != "/manage/compress" {
		return false
	}
	allowed := map[string]bool{"namespace": true}
	wantMethod := "GET"
	switch path {
	case "/v1/descriptions":
		allowed["conversation"] = true
	case "/v1/context":
		allowed["text"], allowed["max_bytes"] = true, true
	case "/manage/compress":
		allowed = map[string]bool{"max_rows": true, "max_input_bytes": true}
		wantMethod = "POST"
	}
	fail := func(code string) { respond(w, nil, errors.New(code)) }
	if r.Method != wantMethod || r.ContentLength != 0 {
		fail("invalid_request")
		return true
	}
	q := r.URL.Query()
	for k, v := range q {
		if !allowed[k] || len(v) != 1 {
			fail("invalid_request")
			return true
		}
	}
	switch path {
	case "/v1/descriptions":
		conversation := q.Get("conversation")
		if conversation == "" || len(conversation) > 512 || strings.ContainsAny(conversation, "\x00\r\n") {
			fail("invalid_conversation")
			return true
		}
		result, err := a.Store.Descriptions(r.Context(), ns, conversation)
		respond(w, result, err)
	case "/v1/context":
		text := q.Get("text")
		maxBytes := 16384
		if strings.TrimSpace(text) == "" || len(text) > 4096 {
			fail("invalid_retrieval_query")
			return true
		}
		if raw := q.Get("max_bytes"); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n < 2 || n > 65536 {
				fail("invalid_retrieval_max_bytes")
				return true
			}
			maxBytes = n
		}
		if a.Retrieval == nil {
			respond(w, retrieval.Response{Status: "unavailable", Context: []retrieval.Item{}, ContextBytes: 2, Coverage: "retrieval_disabled"}, nil)
			return true
		}
		generation, err := a.Store.ReadScopeGeneration(r.Context())
		if err != nil {
			fail("retrieval_unavailable")
			return true
		}
		result, _ := a.Retrieval.Query(r.Context(), ns, generation, retrieval.Request{Text: text, MaxBytes: maxBytes})
		respond(w, result, nil)
	case "/manage/compress":
		rows, input := 100, 4<<20
		for k, destination := range map[string]*int{"max_rows": &rows, "max_input_bytes": &input} {
			if raw := q.Get(k); raw != "" {
				n, e := strconv.Atoi(raw)
				if e != nil {
					fail("invalid_compression_limit")
					return true
				}
				*destination = n
			}
		}
		result, err := a.Store.CompressPayloads(r.Context(), rows, int64(input))
		respond(w, result, err)
	}
	return true
}
