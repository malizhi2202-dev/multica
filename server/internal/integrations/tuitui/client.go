package tuitui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// client is the thin Tuitui robot-API REST seam. Every call is a POST under
// {apiBase}/... authenticated by the appid/secret QUERY parameters (the
// platform accepts no header auth) and answered with a JSON envelope whose
// non-zero errcode means failure. Success responses may carry the delivered
// message id as "msgid" or, for batch-shaped sends, "msgids[]".
type client struct {
	baseURL string // e.g. https://im.live.360.cn:8282/robot
	appID   string
	secret  string
	http    *http.Client
}

const (
	pathSend   = "/message/custom/send"
	pathModify = "/message/custom/modify"
	pathUpload = "/media/upload"

	requestTimeout = 15 * time.Second
	uploadTimeout  = 60 * time.Second
)

func newClient(cfg Config, httpClient *http.Client) *client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &client{
		baseURL: cfg.apiBase(),
		appID:   cfg.AppID,
		secret:  cfg.AppSecret,
		http:    httpClient,
	}
}

// APIError is a platform-level failure: the HTTP call succeeded (or at least
// returned JSON) but errcode != 0.
type APIError struct {
	Path    string
	ErrCode int
	ErrMsg  string
}

func (e *APIError) Error() string {
	if e.ErrMsg != "" {
		return fmt.Sprintf("tuitui: %s: errcode=%d errmsg=%q", e.Path, e.ErrCode, e.ErrMsg)
	}
	return fmt.Sprintf("tuitui: %s: errcode=%d", e.Path, e.ErrCode)
}

// response is the shared reply envelope.
type response struct {
	ErrCode int             `json:"errcode"`
	ErrMsg  string          `json:"errmsg"`
	MsgID   json.RawMessage `json:"msgid"`
	MsgIDs  json.RawMessage `json:"msgids"`
	MediaID string          `json:"media_id"`
	Data    struct {
		MediaID string `json:"media_id"`
	} `json:"data"`
}

// messageID normalizes the two shapes the platform returns delivered ids
// in: a scalar msgid, or a msgids array of {msgid} objects / bare strings
// (reference client.send_interactive's extraction, ported 1:1).
func (r *response) messageID() string {
	if id := rawID(r.MsgID); id != "" {
		return id
	}
	if len(r.MsgIDs) == 0 {
		return ""
	}
	var items []json.RawMessage
	if err := json.Unmarshal(r.MsgIDs, &items); err != nil {
		return ""
	}
	for _, item := range items {
		trimmed := strings.TrimSpace(string(item))
		if trimmed == "" || trimmed == "null" {
			continue
		}
		if trimmed[0] == '{' {
			var obj struct {
				MsgID json.RawMessage `json:"msgid"`
			}
			if json.Unmarshal(item, &obj) == nil {
				if id := rawID(obj.MsgID); id != "" {
					return id
				}
			}
			continue
		}
		if id := rawID(item); id != "" {
			return id
		}
	}
	return ""
}

// rawID renders a JSON scalar (string or number) as its bare string form.
func rawID(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	return trimmed
}

// postJSON sends payload as JSON to path with query-string credentials and
// decodes the envelope. Transport, HTTP-status, JSON and errcode failures are
// all returned as errors.
func (c *client) postJSON(ctx context.Context, path string, payload any) (*response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("tuitui: marshal %s request: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authedURL(path), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tuitui: build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return c.do(req, path)
}

// uploadMedia posts a multipart media upload and returns the platform fid.
// mediaType is the platform "type" form field ("image" / "file" / ...).
func (c *client) uploadMedia(ctx context.Context, mediaType, filename string, content []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("type", mediaType); err != nil {
		return "", fmt.Errorf("tuitui: upload type field: %w", err)
	}
	part, err := mw.CreateFormFile("media", filename)
	if err != nil {
		return "", fmt.Errorf("tuitui: upload form: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return "", fmt.Errorf("tuitui: upload body: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("tuitui: upload close: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authedURL(pathUpload), &buf)
	if err != nil {
		return "", fmt.Errorf("tuitui: build upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.do(req, pathUpload)
	if err != nil {
		return "", err
	}
	fid := resp.MediaID
	if fid == "" {
		fid = resp.Data.MediaID
	}
	if fid == "" {
		return "", fmt.Errorf("tuitui: %s: response has no media_id", pathUpload)
	}
	return fid, nil
}

func (c *client) do(req *http.Request, path string) (*response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tuitui: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("tuitui: %s: read response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tuitui: %s: http %d: %s", path, resp.StatusCode, snippet(body))
	}
	var out response
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("tuitui: %s: decode response: %w (%s)", path, err, snippet(body))
	}
	if out.ErrCode != 0 {
		return &out, &APIError{Path: path, ErrCode: out.ErrCode, ErrMsg: out.ErrMsg}
	}
	return &out, nil
}

// authedURL appends the appid / secret query parameters. The platform API is
// query-authenticated; nothing goes into headers.
func (c *client) authedURL(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return c.baseURL + path + sep + "appid=" + url.QueryEscape(c.appID) + "&secret=" + url.QueryEscape(c.secret)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
