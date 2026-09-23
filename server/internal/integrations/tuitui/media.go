package tuitui

// media.go — the engine.MediaResolver for Tuitui.
//
// The shape is the one the existing channel resolvers established and the
// Router depends on: HasMedia is a pure in-memory look at the payload
// already in hand, ResolveMedia runs detached from the connector ACK path,
// every upload is covered by an intent-ledger row written BEFORE the PUT,
// and nothing is ever deleted inline — a failure anywhere leaves the row for
// the reconciler and leaves the message's placeholder text intact.
//
// What is Tuitui-specific is the source: the callback carries directly
// fetchable http(s) urls — no download API, no credential, no decrypting
// middle like the other channels' flows (reference bridge client
// tuitui_client.py lines 321-383: teams posts expose images[]/files[]
// objects with a url each; plain chats carry the msg_type union whose
// image/mixed images list, voice / video strings, and file {url, name}
// object are the download instructions). So ResolveMedia is a plain GET per
// attachment; the care sits in the fetch constraints instead, mirrored
// from the closest channel resolvers: slack's per-message count cap,
// buffered byte cap and content-type chain, dingtalk's redirect rules and
// non-public-address dial guard, wecom's URL-stripped error text (a signed
// tuitui url is a bearer credential and never reaches a log).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
)

// Tuitui publishes no inbound attachment limits. Keep the adapter's memory
// and remote-I/O budget deliberately inside the shared Router's 45-second
// media deadline: at most 10 attachments per message and 20 MiB buffered per
// attachment (slack's buffered-download numbers), 30 seconds per fetch, 3
// redirects.
const (
	maxAttachmentsPerMessage = 10
	maxInboundMediaBytes     = 20 << 20
	mediaFetchTimeout        = 30 * time.Second
	maxDownloadRedirects     = 3
)

// tuituiMediaSource is one downloadable attachment reference decoded out of
// a frame's Raw envelope: the url to fetch, the kind the sender's client
// declared, and the display name when the platform supplied one.
type tuituiMediaSource struct {
	URL      string
	Kind     channel.MsgType
	Filename string
}

// tuituiMediaSources maps the stamped Raw envelope onto its downloadable
// media references, following the reference bridge client's field union
// exactly (tuitui_client.py lines 321-383): teams posts carry images[] and
// files[] objects each with a url, plain chats carry the msg_type union —
// image/mixed share the images list, voice and video are single url
// strings, file is one {url, name} object. Entries without a url are
// skipped, as the reference skips them; text and link messages carry no
// downloadable attachment.
func tuituiMediaSources(msg channel.InboundMessage) []tuituiMediaSource {
	raw, err := decodeTuituiRaw(msg)
	if err != nil {
		return nil
	}
	var payload eventPayload
	if len(raw.Body.Data) == 0 || json.Unmarshal(raw.Body.Data, &payload) != nil {
		return nil
	}
	var sources []tuituiMediaSource
	switch raw.Body.Event {
	case eventTeamsPostCreate, eventTeamsPostModify:
		for _, img := range payload.Images {
			if img.URL != "" {
				sources = append(sources, tuituiMediaSource{URL: img.URL, Kind: channel.MsgTypeImage, Filename: img.Name})
			}
		}
		for _, f := range payload.Files {
			if f.URL != "" {
				sources = append(sources, tuituiMediaSource{URL: f.URL, Kind: channel.MsgTypeFile, Filename: f.Name})
			}
		}
	default:
		switch payload.MsgType {
		case "mixed", "image":
			for _, img := range payload.Images {
				if img.URL != "" {
					sources = append(sources, tuituiMediaSource{URL: img.URL, Kind: channel.MsgTypeImage, Filename: img.Name})
				}
			}
		case "voice":
			if payload.Voice != "" {
				sources = append(sources, tuituiMediaSource{URL: payload.Voice, Kind: channel.MsgTypeAudio})
			}
		case "video":
			if payload.Video != "" {
				sources = append(sources, tuituiMediaSource{URL: payload.Video, Kind: channel.MsgTypeVideo})
			}
		case "file":
			if payload.File.URL != "" {
				sources = append(sources, tuituiMediaSource{URL: payload.File.URL, Kind: channel.MsgTypeFile, Filename: payload.File.Name})
			}
		}
	}
	return sources
}

// mediaStorage is the slice of storage.Storage this resolver drives.
// ObjectURL is a pure function of configuration, which is what lets the
// intent ledger persist the object's URL before the object exists.
type mediaStorage interface {
	Upload(ctx context.Context, key string, data []byte, contentType string, filename string) (string, error)
	ObjectURL(key string) string
}

type mediaResolver struct {
	storage mediaStorage
	ledger  engine.MediaIntentLedger
	fetch   *http.Client
	// maxBytes caps one downloaded body. Production is maxInboundMediaBytes
	// via NewMediaResolver; tests lower it to prove the over-cap refusal
	// without transferring a real 20 MiB body (the same reason the WeCom
	// fetch helper takes its ceiling as a parameter).
	maxBytes int64
	logger   *slog.Logger
}

var _ engine.MediaResolver = (*mediaResolver)(nil)

// NewMediaResolver builds the Tuitui MediaResolver. storage and ledger are
// both required — without either there is nothing durable to point an
// attachment at, and ResolveMedia degrades to leaving the placeholder text
// in place (the cmd/server wiring only builds a resolver when a storage
// backend exists).
func NewMediaResolver(store mediaStorage, ledger engine.MediaIntentLedger, logger *slog.Logger) engine.MediaResolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &mediaResolver{
		storage: store,
		ledger:  ledger,
		// Never a bare http.Client: the url being fetched came off the wire,
		// and this client refuses to connect to anything that is not public
		// internet (see newMediaHTTPClient below).
		fetch:    newMediaHTTPClient(),
		maxBytes: maxInboundMediaBytes,
		logger:   logger,
	}
}

// HasMedia reports whether this frame references downloadable media. It
// runs synchronously on the connector ACK path and decides whether the
// message pays for a media deadline, a deferred run and a semaphore slot at
// all, so it stays a pure decode of bytes already in hand.
func (m *mediaResolver) HasMedia(msg channel.InboundMessage) bool {
	return len(tuituiMediaSources(msg)) > 0
}

// ResolveMedia downloads and stores every attachment on the message,
// returning it with a MediaRef per object that landed. Attachments are
// independent: one that fails does not stop the rest, the failure is only
// logged, and the stored placeholder text stays (best-effort semantics of
// engine.MediaResolver).
func (m *mediaResolver) ResolveMedia(ctx context.Context, inst engine.ResolvedInstallation, _ engine.ResolvedIdentity, _ pgtype.UUID, chatMessageID pgtype.UUID, msg channel.InboundMessage) channel.InboundMessage {
	sources := tuituiMediaSources(msg)
	if len(sources) == 0 {
		return msg
	}
	if m.storage == nil || m.ledger == nil || m.maxBytes <= 0 {
		m.logWarn(msg, errors.New("media dependency missing"))
		return msg
	}
	if len(sources) > maxAttachmentsPerMessage {
		m.logWarn(msg, fmt.Errorf("%d attachments exceed the limit of %d; extra attachments skipped", len(sources), maxAttachmentsPerMessage))
		sources = sources[:maxAttachmentsPerMessage]
	}
	for i, src := range sources {
		if err := ctx.Err(); err != nil {
			// The Router discards every ref once the media deadline passes,
			// so there is nothing left to win by starting another file.
			m.logWarn(msg, fmt.Errorf("media budget spent after %d of %d attachments: %w", i, len(sources), err))
			break
		}
		fileCtx, cancel := context.WithTimeout(ctx, mediaFetchBudget(ctx, len(sources)-i))
		ref, err := m.ingestOne(fileCtx, inst, chatMessageID, i, src)
		cancel()
		if err != nil {
			// The url never reaches this log line: err has had the signed
			// url stripped out of it (stripURL), and no other path echoes
			// the source back.
			m.logger.Warn("tuitui media ingest failed",
				"installation_id", util.UUIDToString(inst.ID),
				"message_id", msg.MessageID,
				"attachment", i,
				"kind", string(src.Kind),
				"err", err)
			continue
		}
		msg.MediaRefs = append(msg.MediaRefs, ref)
	}
	return msg
}

// mediaFetchBudget caps one attachment's download and upload. The flat
// timeout bounds a single stalled transfer, but several of them in series
// would run past the Router's media deadline — and the Router drops EVERY
// ref once that deadline passes, so one slow attachment would cost the whole
// message its attachments. Sharing what is left of the budget over the
// attachments still to fetch keeps a slow early one from starving the rest
// (the slack resolver's reasoning, copied to this buffer-per-message path).
func mediaFetchBudget(ctx context.Context, remaining int) time.Duration {
	if remaining < 1 {
		remaining = 1
	}
	budget := mediaFetchTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if share := time.Until(deadline) / time.Duration(remaining); share < budget {
			budget = share
		}
	}
	return budget
}

// ingestOne carries a single attachment from url to MediaRef. The ledger row
// goes first: from that point on every failure — download, upload, a crash —
// leaves an intent the reconciler settles, and nothing here deletes anything.
func (m *mediaResolver) ingestOne(ctx context.Context, inst engine.ResolvedInstallation, chatMessageID pgtype.UUID, index int, src tuituiMediaSource) (channel.MediaRef, error) {
	key := mediaObjectKey(inst, chatMessageID, src, index)
	link := m.storage.ObjectURL(key)
	owned, err := m.ledger.RecordPendingMediaObject(ctx, engine.RecordPendingMediaObjectParams{
		StorageKey:     key,
		WorkspaceID:    inst.WorkspaceID,
		ChatMessageID:  chatMessageID,
		StorageURL:     link,
		InstallationID: inst.ID,
	})
	if err != nil {
		// No durable intent, no upload — the fail-safe direction.
		return channel.MediaRef{}, fmt.Errorf("record media intent: %w", err)
	}
	if !owned {
		// The reconciler owns this key; never resurrect it.
		return channel.MediaRef{}, errors.New("media key owned by reconciler")
	}
	data, contentType, err := m.download(ctx, src.URL)
	if err != nil {
		return channel.MediaRef{}, err
	}
	filename := mediaDisplayName(src.Filename, src.Kind, index, contentType)
	if _, err := m.storage.Upload(ctx, key, data, contentType, filename); err != nil {
		// The store may still be processing the PUT; the intent row covers
		// the object either way.
		return channel.MediaRef{}, fmt.Errorf("upload media: %w", err)
	}
	return channel.MediaRef{
		Type:       src.Kind,
		StorageKey: key,
		StorageURL: link,
		Filename:   filename,
		MimeType:   contentType,
		SizeBytes:  int64(len(data)),
	}, nil
}

// mediaObjectKey names the object. It is derived from the CHAT message
// rather than the platform message for the reason the slack and wecom
// resolvers document: one platform message can be ingested twice (the
// inbound dedup claim is reclaimable once stale), and a shared key would run
// the second ingest into the first one's ledger row. The url is hashed in,
// never stored — a signed url is short-lived and must not name a durable
// object path verbatim.
func mediaObjectKey(inst engine.ResolvedInstallation, chatMessageID pgtype.UUID, src tuituiMediaSource, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d",
		util.UUIDToString(chatMessageID), src.URL, index)))
	return path.Join(
		"workspaces",
		util.UUIDToString(inst.WorkspaceID),
		"tuitui",
		util.UUIDToString(inst.ID),
		hex.EncodeToString(sum[:]),
	)
}

// download GETs one frame-supplied url under the resolver's ceilings. Errors
// carry the reason (refused address, http status, oversize body, stalled
// server) but never the url itself.
func (m *mediaResolver) download(ctx context.Context, rawURL string) ([]byte, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		// url.Parse's error text quotes the whole input, and the input here
		// is a live signed url. Say what was wrong, not what it was.
		return nil, "", errors.New("invalid media download URL")
	}
	if err := validateDownloadURL(parsed); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", errors.New("build media download request failed")
	}
	resp, err := m.fetch.Do(req)
	if err != nil {
		// net/http's *url.Error includes the full request URL. Tuitui media
		// urls carry a signature query, so unwrap it before the caller logs
		// the error and never persist the signed query string.
		return nil, "", fmt.Errorf("download media request failed: %w", stripURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("download media: http %d", resp.StatusCode)
	}
	if resp.ContentLength > m.maxBytes {
		return nil, "", fmt.Errorf("media exceeds the %d MiB limit", m.maxBytes>>20)
	}
	// LimitReader with one byte of headroom: reading exactly the cap cannot
	// tell "the file is exactly at the limit" from "there is more coming".
	data, err := io.ReadAll(io.LimitReader(resp.Body, m.maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read media: %w", err)
	}
	if int64(len(data)) > m.maxBytes {
		return nil, "", fmt.Errorf("media exceeds the %d MiB limit", m.maxBytes>>20)
	}
	contentType := resp.Header.Get("Content-Type")
	if semi := strings.IndexByte(contentType, ';'); semi >= 0 {
		contentType = strings.TrimSpace(contentType[:semi])
	}
	if contentType == "" {
		sniff := data
		if len(sniff) > 512 {
			sniff = sniff[:512]
		}
		contentType = http.DetectContentType(sniff)
		if semi := strings.IndexByte(contentType, ';'); semi >= 0 {
			contentType = strings.TrimSpace(contentType[:semi])
		}
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return data, contentType, nil
}

func validateDownloadURL(parsed *url.URL) error {
	if parsed == nil {
		return errors.New("invalid media download URL")
	}
	// Scheme first: this is what refuses a file:// or gopher:// before any
	// other shape question — without it a file:// URL (whose Host is empty)
	// would be answered with the vaguer shape message.
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("invalid media download URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("invalid media download URL shape")
	}
	return nil
}

// stripURLError removes the request URL from a transport error before it can
// be logged: the media url is a signed, fetchable link, and a DNS hiccup, a
// TCP reset, a TLS error or the download timeout must not each write one
// into the application log. The wrapped cause carries the same diagnosis
// with none of the credential (the wecom media downloader's rule).
func stripURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// mediaDisplayName picks the stored display name: the platform's own name
// when it is a usable filename (plain-chat file messages and teams file /
// image objects carry one), otherwise a generated unique name — the
// attachment's position is in it so several images in one message never
// share a name (the slack / wecom fallback convention).
func mediaDisplayName(srcName string, kind channel.MsgType, index int, contentType string) string {
	if name := cleanMediaName(srcName); name != "" {
		return name
	}
	prefix := "tuitui-file"
	switch kind {
	case channel.MsgTypeImage:
		prefix = "tuitui-image"
	case channel.MsgTypeAudio:
		prefix = "tuitui-audio"
	case channel.MsgTypeVideo:
		prefix = "tuitui-video"
	}
	return fmt.Sprintf("%s-%d%s", prefix, index+1, mediaExtension(contentType))
}

// cleanMediaName reduces a platform-supplied name to a single safe path
// segment (the slack cleanFileName rule; the name lands in
// attachments.filename and must survive Postgres TEXT and download
// headers).
func cleanMediaName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	// path.Base hands back ".." and "..." unchanged; a name made only of
	// dots is not a filename.
	if strings.Trim(name, ".") == "" || name == "/" {
		return ""
	}
	return name
}

// mediaExtension picks a file extension for a content type, preferring the
// familiar spelling over whatever the mime database happens to list first
// (image/jpeg resolves to ".jfif" on some systems), and pinning the common
// types a slim container image's mime database may not carry.
func mediaExtension(contentType string) string {
	if semi := strings.IndexByte(contentType, ';'); semi >= 0 {
		contentType = strings.TrimSpace(contentType[:semi])
	}
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	}
	if exts, err := mime.ExtensionsByType(contentType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}

func (m *mediaResolver) logWarn(msg channel.InboundMessage, err error) {
	m.logger.Warn("tuitui media resolve skipped", "message_id", msg.MessageID, "error", err)
}

// ---- guarded fetch client ----
//
// The media fetcher is pointed at an address by somebody else: the url came
// off the callback socket, and the fetch runs from inside the deployment's
// network. So the guard is on the CONNECTION, not on the URL string:
// resolve the host ourselves, reject every non-public answer, and dial the
// validated IP directly so DNS rebinding cannot redirect the connection
// into the local network. Proxy use is disabled because a proxy would
// resolve the target again and bypass this guarantee. The shape is the
// DingTalk media resolver's, copied rather than loosened.

func newMediaHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&publicDownloadDialer{
		resolver: net.DefaultResolver,
		dialer:   &net.Dialer{Timeout: mediaFetchTimeout},
	}).DialContext
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxDownloadRedirects {
				return errors.New("too many redirects")
			}
			// net/http derives Referer from the previous URL. The query
			// string is a short-lived bearer credential, so it must never
			// cross into a redirect request, including an otherwise-allowed
			// same-origin hop.
			req.Header.Del("Referer")
			if err := validateDownloadURL(req.URL); err != nil {
				return err
			}
			previous := via[len(via)-1].URL
			if strings.EqualFold(previous.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
				return errors.New("disallowed HTTPS download redirect downgrade")
			}
			if strings.EqualFold(req.URL.Scheme, "http") && !sameDownloadOrigin(previous, req.URL) {
				return errors.New("disallowed cross-origin HTTP download redirect")
			}
			return nil
		},
	}
}

type publicDownloadDialer struct {
	resolver *net.Resolver
	dialer   *net.Dialer
}

func (d *publicDownloadDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid download target address")
	}
	addrs, err := d.lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, errors.New("resolve download target failed")
	}
	for _, addr := range addrs {
		if !isPublicDownloadAddress(addr) {
			return nil, errors.New("blocked non-public download target")
		}
	}
	for _, addr := range addrs {
		conn, err := d.dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("connect to download target failed")
}

func (d *publicDownloadDialer) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	addrs, err := d.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	return addrs, nil
}

// nonPublicDownloadPrefixes is the IANA special-purpose space beyond what
// netip's own predicates catch — the same table the DingTalk resolver
// refuses against (RFC 6598 CGNAT included: a tailnet peer is a machine
// inside the trust boundary reachable by IP with no credential).
var nonPublicDownloadPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// IPv6 transition mechanisms can encapsulate an otherwise-blocked IPv4
	// destination in an address that netip classifies as global unicast. The
	// download client does not need these legacy/local transition ranges, so
	// fail closed instead of attempting to decode every deployment-specific
	// mapping and risking a route into loopback or RFC1918 space.
	netip.MustParsePrefix("::/96"),          // deprecated IPv4-compatible
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001::/32"),      // Teredo
	netip.MustParsePrefix("2001:2::/48"),    // benchmarking
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2001:10::/28"), // deprecated ORCHID
	netip.MustParsePrefix("2001:20::/28"), // ORCHIDv2
	netip.MustParsePrefix("2002::/16"),    // 6to4
	netip.MustParsePrefix("3fff::/20"),    // documentation
}

var wellKnownNAT64Prefix = netip.MustParsePrefix("64:ff9b::/96")

func isPublicDownloadAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	if wellKnownNAT64Prefix.Contains(addr) {
		// The standard NAT64 prefix carries an IPv4 address in its low 32
		// bits. Permit a synthesized public target, but apply the complete
		// IPv4 deny policy to prevent an attacker-controlled AAAA record
		// from smuggling loopback or RFC1918 through an apparently global
		// IPv6 value.
		raw := addr.As16()
		return isPublicDownloadAddress(netip.AddrFrom4([4]byte(raw[12:16])))
	}
	for _, prefix := range nonPublicDownloadPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func sameDownloadOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
