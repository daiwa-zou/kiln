package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

// Filing into the review queue from outside the pipeline, and promoting a
// proposed correction to a pinned one.
//
// An agent may ask a question about a page or propose a correction to it.
// Nothing it sends changes a page: a filing is a review item, read by a human,
// and the one way from a proposal into a prompt is POST …/promote, which needs
// the write scope and carries the text the person confirmed. A key minted for
// an agent carries "propose:<bench>" rather than write, and reaches only the
// two filing routes below (proposeRoutes), only for that bench.

// Filing limits. Rejected when exceeded, never truncated: a cut-off question
// or correction says something its author did not.
const (
	maxFilingBytes    = 64 << 10 // the whole request body
	maxFilingTitle    = 200      // runes
	maxFilingPageRef  = 512      // bytes
	maxProposedBytes  = 16 << 10 // a correction's text, same ceiling as a pinned one
	maxRationaleRunes = 2000
)

// filingKinds is the whole set of review kinds a caller may file. Hard-coded on
// purpose: the kind decides what resolving an item does, and an approved
// "deletion" review is what authorizes the next build's delete cascade. A kind
// outside this list is a 400, whatever the caller's scope.
var filingKinds = []string{"uncertain", "contradiction", "correction"}

// proposeRoutes are the route patterns a propose-scoped key may reach, as chi
// reports them. Matching on chi's own pattern rather than on the URL means
// there is no second parser to disagree with the router about %2F, "//", a
// trailing slash or case: a request either reaches one of these handlers or it
// does not, and the drift test walks the real router to keep this list honest.
var proposeRoutes = map[string]bool{
	"/api/v1/workspaces/{workspace}/reviews":             true,
	"/api/v1/workspaces/{workspace}/gaps/{slug}/request": true,
}

// proposeGate answers the auth middleware's question for a propose-scoped
// token: does this request reach a filing route, for a bench the token is
// pinned to?
//
// The middleware runs above the /workspaces/{workspace} subrouter, where chi
// has matched only "/api/v1/workspaces/{workspace}/*", so the final pattern is
// found by asking the finished router (mux) with a fresh routing context, on
// the same path chi routes on. The workspace reference is resolved with the
// caller's own visibility, exactly as the handler will resolve it.
func (s *Server) proposeGate(mux chi.Routes) func(*http.Request, auth.Identity) bool {
	return func(r *http.Request, id auth.Identity) bool {
		if r.Method != http.MethodPost {
			return false
		}
		path := r.URL.RawPath
		if path == "" {
			path = r.URL.Path
		}
		rctx := chi.NewRouteContext()
		if !proposeRoutes[mux.Find(rctx, r.Method, path)] {
			return false
		}
		ws, err := s.Store.ResolveWorkspace(r.Context(),
			refFromParam(rctx.URLParam("workspace")), id.UserID, id.Admin)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) && s.Log != nil {
				s.Log.Error("propose gate: resolve workspace", "err", err)
			}
			return false
		}
		return id.CanProposeIn(ws.ID)
	}
}

// guardFiling is guardWrite for the filing routes: it adds the bench pin. The
// middleware already refused a propose token for any other bench; this is the
// same check made against the workspace the handler actually resolved, so the
// two can never disagree about which bench a filing lands in.
func (s *Server) guardFiling(w http.ResponseWriter, r *http.Request, maxBytes int64) (store.WorkspaceRow, bool) {
	ws, _, ok := s.guardWrite(w, r, maxBytes)
	if !ok {
		return store.WorkspaceRow{}, false
	}
	if id, found := auth.FromContext(r.Context()); found && s.Auth != nil &&
		!id.HasScope(auth.ScopeWrite) && !id.CanProposeIn(ws.ID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this key cannot file into this bench"})
		return store.WorkspaceRow{}, false
	}
	return ws, true
}

// filingOrigin says who filed: an API key (its name, for display, and its id,
// which the per-key cap counts), else a browser session, else an authless
// (loopback-only) instance. kind is what the caps and the inbox badge read;
// the name is a label the key's minter chose and decides nothing.
func filingOrigin(r *http.Request) (origin, kind, tokenID string) {
	id, found := auth.FromContext(r.Context())
	switch {
	case !found:
		return "local", store.OriginLocal, ""
	case id.TokenID != "":
		name := cleanLine(id.TokenName)
		if name == "" {
			name = "unnamed key"
		}
		return name, store.OriginKey, id.TokenID
	default:
		return "session", store.OriginSession, ""
	}
}

// handleReviewFile files a question about a page, or a proposed correction to
// it, into the review queue. Never a change to the page.
func (s *Server) handleReviewFile(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardFiling(w, r, maxFilingBytes)
	if !ok {
		return
	}
	var body struct {
		Kind   string `json:"kind"`
		Page   string `json:"page"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Text   string `json:"text"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	bad := func(msg string) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
	}

	if !slices.Contains(filingKinds, body.Kind) {
		bad("kind must be one of " + strings.Join(filingKinds, ", "))
		return
	}
	page := strings.TrimSpace(body.Page)
	if page == "" || len(page) > maxFilingPageRef {
		bad("page is required: the slug or path of a page in this bench")
		return
	}

	p := store.FileReviewParams{WorkspaceID: ws.ID, Kind: body.Kind, PageRef: page}
	p.Origin, p.OriginKind, p.OriginToken = filingOrigin(r)
	if body.Kind == "correction" {
		// A correction carries its text and the reason for it. The title is the
		// store's to derive from the page, so the field must not be sent.
		if body.Title != "" {
			bad("a correction takes text and detail (the rationale), not a title")
			return
		}
		p.Proposed = strings.TrimSpace(cleanBlock(body.Text))
		p.Detail = strings.TrimSpace(cleanBlock(body.Detail))
		switch {
		case p.Proposed == "":
			bad("text is required: the correction you propose")
			return
		case len(p.Proposed) > maxProposedBytes:
			bad(fmt.Sprintf("text is over %d bytes; corrections are injected into every prompt for the page, so keep it to what must change", maxProposedBytes))
			return
		case utf8.RuneCountInString(p.Detail) > maxRationaleRunes:
			bad(fmt.Sprintf("detail (the rationale) is over %d characters", maxRationaleRunes))
			return
		}
	} else {
		if body.Text != "" {
			bad("text is for proposed corrections; a question takes a title and detail")
			return
		}
		p.Title = cleanLine(body.Title)
		p.Detail = strings.TrimSpace(cleanBlock(body.Detail))
		switch {
		case p.Title == "":
			bad("title is required: the question, in one line")
			return
		case utf8.RuneCountInString(p.Title) > maxFilingTitle:
			bad(fmt.Sprintf("title is over %d characters", maxFilingTitle))
			return
		case len(p.Detail) > maxCorrectionBytes:
			bad(fmt.Sprintf("detail is over %d bytes", maxCorrectionBytes))
			return
		}
	}

	id, outcome, err := s.Writes.FileAgentReview(r.Context(), p)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "page not found in this bench"})
		return
	case errors.Is(err, store.ErrAgentCap):
		writeAgentCap(w)
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	if outcome == store.FiledAlreadyOpen {
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": outcome})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "status": outcome})
}

// writeAgentCap answers a filing refused by the open-item caps. Both filing
// routes share it, so a gap request and a question say the same thing.
func writeAgentCap(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "3600")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf(
		"too many open items filed by keys (at most %d per key, %d per bench); "+
			"they clear as people answer them", store.AgentOpenCapPerKey, store.AgentOpenCapPerBench)})
}

// handleReviewPromote pins a proposed correction with the text a human
// confirmed. Not a filing route: it needs the write scope (a propose key is
// refused by the middleware), it is never an MCP tool, and the text it pins is
// the one in this request -- what the person read -- not the proposal.
func (s *Server) handleReviewPromote(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := s.guardWrite(w, r, maxFilingBytes)
	if !ok {
		return
	}
	reviewID := chi.URLParam(r, "id")
	var body struct {
		Text string `json:"text"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	text := strings.TrimSpace(cleanBlock(body.Text))
	switch {
	case text == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required: the correction to pin"})
		return
	case len(text) > maxCorrectionBytes:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("text is over %d bytes", maxCorrectionBytes)})
		return
	}
	// A malformed id is the same answer as a missing one, not a database error.
	if _, err := uuid.Parse(reviewID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no open proposed correction with that id"})
		return
	}

	id, err := s.Writes.PromoteReview(r.Context(), ws.ID, reviewID, text, uid)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no open proposed correction with that id"})
		return
	case errors.Is(err, store.ErrPageGone):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the page was deleted after this was proposed; nothing was pinned"})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "status": "pinned"})
}

// Filed text is shown to people in the inbox and, once promoted, injected into
// prompts, so what it may contain depends on where it goes.
//
// cleanLine is for one-line fields (a title, a key name): every kind of space
// or line/paragraph separator becomes one space, and control characters,
// bidi controls, tag characters and private-use code points are dropped. A
// title cannot break onto a forged second line, or reorder itself on screen.
//
// cleanBlock is for multi-line fields (detail, correction text): it keeps the
// line structure (\n and \t) and drops only what is invisible and can deceive
// -- other control characters, bidi controls, the tag block and the zero-width
// characters that only hide or split text. Joiners (ZWJ/ZWNJ) are kept:
// scripts and emoji need them, and they cannot reorder or hide text.
//
// Both drop variation selectors that have nothing to vary: the supplementary
// ones always (U+E0100–E01EF, ideographic variants a reader never needs from
// an agent), and the basic ones (U+FE00–FE0F) after ASCII or at the start,
// where they can only smuggle invisible bytes; after an emoji or CJK base
// they choose a glyph and stay.
func cleanLine(s string) string {
	s = strings.ToValidUTF8(s, string(rune(0xFFFD)))
	var b strings.Builder
	space := false
	prev := rune(-1)
	for _, r := range s {
		switch {
		case unicode.IsSpace(r) || unicode.In(r, unicode.Zl, unicode.Zp):
			space = true
			continue
		case unicode.IsControl(r) || deceptive(r) || unicode.Is(unicode.Co, r) ||
			strayVariation(prev, r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

func cleanBlock(s string) string {
	s = strings.ToValidUTF8(s, string(rune(0xFFFD)))
	var b strings.Builder
	prev := rune(-1)
	for _, r := range s {
		if r != '\n' && r != '\t' &&
			(unicode.IsControl(r) || deceptive(r) || strayVariation(prev, r)) {
			continue
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

// deceptive reports the invisible code points that change how text reads
// without appearing in it: the Unicode bidi controls (which include the
// U+202A–202E embeddings and U+2066–2069 isolates), the tag block, and the
// zero-width space, word joiner, BOM and soft hyphen, which hide or split
// words without a visible trace.
func deceptive(r rune) bool {
	switch r {
	case 0x200B, 0x2060, 0xFEFF, 0x00AD:
		return true
	}
	return unicode.Is(unicode.Bidi_Control, r) || (r >= 0xE0000 && r <= 0xE007F)
}

// strayVariation reports a variation selector with no base worth varying;
// see cleanLine.
func strayVariation(prev, r rune) bool {
	switch {
	case r >= 0xE0100 && r <= 0xE01EF:
		return true
	case r >= 0xFE00 && r <= 0xFE0F:
		return prev < 0x80
	}
	return false
}

// refFromParam decodes a raw {workspace} path segment into a workspace
// reference; see workspaceRef.
func refFromParam(raw string) string {
	ref := raw
	if dec, err := url.PathUnescape(ref); err == nil {
		ref = dec
	}
	return strings.Replace(ref, "~", "/", 1)
}
