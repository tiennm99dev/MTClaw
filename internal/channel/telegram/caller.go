package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	ta "github.com/mymmrac/telego/telegoapi"
)

// apiCaller is telego's net/http caller with one difference: an HTTP 5xx
// comes back as a *ta.Error carrying the status code, instead of telego's
// own untyped "internal server error" string. sendOne's transient-retry
// branch matches on *ta.Error, so without this a 502/503 from Telegram would
// never be retried.
type apiCaller struct {
	client *http.Client
}

func newAPICaller(client *http.Client) ta.Caller { return apiCaller{client: client} }

// Call implements ta.Caller.
func (c apiCaller) Call(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
	var body io.Reader
	switch {
	case data.BodyRaw != nil:
		body = bytes.NewReader(data.BodyRaw)
	case data.BodyStream != nil:
		body = data.BodyStream
	default:
		return nil, errors.New("body is not provided")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("http create request: %w", err)
	}
	req.Header.Set(ta.ContentTypeHeader, data.ContentType)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusInternalServerError {
		return nil, &ta.Error{ErrorCode: resp.StatusCode, Description: http.StatusText(resp.StatusCode)}
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	apiResp := &ta.Response{}
	if err := json.Unmarshal(raw, apiResp); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	return apiResp, nil
}
