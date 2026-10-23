package osclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
)

// ErrNoSeqNo marks a 2xx GET whose body lacks integer _seq_no/_primary_term.
var ErrNoSeqNo = errors.New("response has no integer _seq_no/_primary_term")

// Replay returns an attempt that clones req onto ctx, replays GetBody, and calls client.Do.
func Replay(client *http.Client, req *http.Request) func(context.Context) (*http.Response, error) {
	return func(ctx context.Context) (*http.Response, error) {
		return send(ctx, client, req)
	}
}

func send(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	c := req.Clone(ctx)
	if req.GetBody != nil {
		b, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("preparing request body: %w", err)
		}
		c.Body = b
	}
	return client.Do(c)
}

// Upsert returns an attempt that GETs spec.Path and then PUTs spec, conditionally
// on the document's _seq_no and _primary_term. unchanged, when non-nil, sees the
// 2xx GET body; true returns that response and sends no PUT.
//
//nolint:gocritic // hugeParam: RequestSpec is the documented value-typed public API.
func Upsert(client *http.Client, endpoint string, spec RequestSpec, unchanged func(body []byte) bool) (func(context.Context) (*http.Response, error), error) {
	getReq, err := BuildRequest(endpoint, RequestSpec{
		Method: http.MethodGet, Path: spec.Path, Query: spec.Query, Headers: spec.Headers,
	})
	if err != nil {
		return nil, err
	}
	putReq, err := BuildRequest(endpoint, spec)
	if err != nil {
		return nil, err
	}

	return func(ctx context.Context) (*http.Response, error) {
		resp, err := send(ctx, client, getReq)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return send(ctx, client, putReq)
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return resp, nil
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading GET body: %w", err)
		}
		if unchanged != nil && unchanged(body) {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, nil
		}

		var doc struct {
			SeqNo       *int64 `json:"_seq_no"`
			PrimaryTerm *int64 `json:"_primary_term"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("%w: GET %s: %w", ErrNoSeqNo, spec.Path, err)
		}
		if doc.SeqNo == nil || doc.PrimaryTerm == nil {
			return nil, fmt.Errorf("%w: GET %s", ErrNoSeqNo, spec.Path)
		}

		cond := spec
		cond.Query = make(map[string]string, len(spec.Query)+2)
		maps.Copy(cond.Query, spec.Query)
		cond.Query["if_seq_no"] = strconv.FormatInt(*doc.SeqNo, 10)
		cond.Query["if_primary_term"] = strconv.FormatInt(*doc.PrimaryTerm, 10)
		condReq, err := BuildRequest(endpoint, cond)
		if err != nil {
			return nil, err
		}
		return send(ctx, client, condReq)
	}, nil
}
