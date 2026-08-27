package imap

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
)

// errValidityChanged says a folder was recreated under us: its uids now mean
// something else entirely, so anything measured against the old UIDVALIDITY has
// to be thrown away rather than trusted.
var errValidityChanged = errors.New("imap: uidvalidity changed")

// stateVersion guards the format. A state this build cannot read is reported
// as expired, which costs one reconcile and is the only thing that does.
const stateVersion = 1

// gzipAbove is the size past which the state is compressed. A normal account
// is a few hundred bytes; only a pathological one with many folders and many
// expunge holes gets near this.
const gzipAbove = 32 << 10

// folderState is what we knew about one folder at the end of the last pass.
//
// UIDs is the load-bearing field. IMAP has no server-side change log, and
// go-imap/v2 cannot do QRESYNC, so nothing on the wire will tell us which
// messages vanished. The provider cannot see the index either. Remembering the
// UID set we last reported is what makes it self-sufficient — and it is what
// turns a UIDVALIDITY reset or a deleted folder into exact Removed ids instead
// of an expired state and a whole-account reconcile.
type folderState struct {
	UIDVal  uint32 `json:"v"`
	UIDNext uint32 `json:"x"`
	ModSeq  uint64 `json:"m,omitempty"` // 0 = the server has no CONDSTORE
	Count   uint32 `json:"c"`
	// Unseen is what makes a read/unread change visible on a server with no
	// CONDSTORE: it is the one flag STATUS reports, and it is the flag users
	// actually notice moving.
	Unseen uint32 `json:"n,omitempty"`
	UIDs   string `json:"u"` // IMAP range string: "1:5000,5002:9013"
	// Scan is the pass counter at the last full flag rescan, for servers with
	// no CONDSTORE where flag changes are otherwise invisible.
	Scan int `json:"s,omitempty"`

	// The flags we last reported, one uid range per flag.
	//
	// Without these a delta cannot tell a flag that changed from a flag it
	// merely re-read, so every poll would report every recently-touched message
	// as updated — churning the index and the FTS triggers on a quiet account.
	// Range-encoded like UIDs, and cheap for the same reason: Seen is normally
	// "nearly everything" and the rest are normally almost empty.
	Seen     string `json:"sn,omitempty"`
	Flagged  string `json:"fl,omitempty"`
	Answered string `json:"an,omitempty"`
	Draft    string `json:"dr,omitempty"`
}

// flagSets is folderState's flag ranges, decoded for lookup.
type flagSets struct {
	seen, flagged, answered, draft map[imapv2.UID]bool
}

func (f folderState) flagSets() (flagSets, error) {
	var out flagSets
	var err error
	if out.seen, err = uidIndex(f.Seen); err != nil {
		return out, err
	}
	if out.flagged, err = uidIndex(f.Flagged); err != nil {
		return out, err
	}
	if out.answered, err = uidIndex(f.Answered); err != nil {
		return out, err
	}
	if out.draft, err = uidIndex(f.Draft); err != nil {
		return out, err
	}
	return out, nil
}

func newFlagSets() flagSets {
	return flagSets{
		seen:     map[imapv2.UID]bool{},
		flagged:  map[imapv2.UID]bool{},
		answered: map[imapv2.UID]bool{},
		draft:    map[imapv2.UID]bool{},
	}
}

// flagsOf is what we last knew about one message.
func (f flagSets) flagsOf(uid imapv2.UID) model.Flags {
	return model.Flags{
		Unread:   !f.seen[uid],
		Flagged:  f.flagged[uid],
		Draft:    f.draft[uid],
		Answered: f.answered[uid],
	}
}

// set records a message's current flags.
func (f flagSets) set(uid imapv2.UID, fl model.Flags) {
	assign(f.seen, uid, !fl.Unread)
	assign(f.flagged, uid, fl.Flagged)
	assign(f.answered, uid, fl.Answered)
	assign(f.draft, uid, fl.Draft)
}

// forget drops a message entirely, for one that was expunged.
func (f flagSets) forget(uid imapv2.UID) {
	delete(f.seen, uid)
	delete(f.flagged, uid)
	delete(f.answered, uid)
	delete(f.draft, uid)
}

// encodeInto writes the sets back onto a folderState.
func (f flagSets) encodeInto(fs *folderState) {
	fs.Seen = encodeUIDs(keysOf(f.seen))
	fs.Flagged = encodeUIDs(keysOf(f.flagged))
	fs.Answered = encodeUIDs(keysOf(f.answered))
	fs.Draft = encodeUIDs(keysOf(f.draft))
}

func assign(m map[imapv2.UID]bool, uid imapv2.UID, on bool) {
	if on {
		m[uid] = true
	} else {
		delete(m, uid)
	}
}

func keysOf(m map[imapv2.UID]bool) []imapv2.UID {
	out := make([]imapv2.UID, 0, len(m))
	for u := range m {
		out = append(out, u)
	}
	return out
}

// mailState is the whole account's delta state, serialised into the opaque
// string sync_state already stores as TEXT.
type mailState struct {
	V       int                    `json:"v"`
	Pass    int                    `json:"p,omitempty"`
	Folders map[string]folderState `json:"f"`
}

func newMailState() mailState {
	return mailState{V: stateVersion, Folders: map[string]folderState{}}
}

// String serialises the state, compressing a large one.
func (s mailState) String() string {
	s.V = stateVersion
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	if len(b) < gzipAbove {
		return string(b)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil || zw.Close() != nil {
		return string(b)
	}
	return "z:" + base64.RawStdEncoding.EncodeToString(buf.Bytes())
}

// parseMailState reads a state token. An empty, malformed or older-version
// token yields ok=false, which the caller turns into ErrStateExpired.
func parseMailState(s string) (mailState, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return mailState{}, false
	}
	if rest, found := strings.CutPrefix(s, "z:"); found {
		raw, err := base64.RawStdEncoding.DecodeString(rest)
		if err != nil {
			return mailState{}, false
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return mailState{}, false
		}
		b, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return mailState{}, false
		}
		s = string(b)
	}
	var st mailState
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return mailState{}, false
	}
	if st.V != stateVersion || st.Folders == nil {
		return mailState{}, false
	}
	return st, true
}

// ---------------------------------------------------------------------------
// UID range codec

// encodeUIDs renders a sorted UID list as an IMAP sequence set. A folder whose
// mail has never been expunged collapses to a single range, which is why the
// whole account's state stays small.
func encodeUIDs(uids []imapv2.UID) string {
	if len(uids) == 0 {
		return ""
	}
	sorted := append([]imapv2.UID(nil), uids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var b strings.Builder
	start, prev := sorted[0], sorted[0]
	flush := func() {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uint64(start), 10))
		if prev != start {
			b.WriteByte(':')
			b.WriteString(strconv.FormatUint(uint64(prev), 10))
		}
	}
	for _, u := range sorted[1:] {
		if u == prev {
			continue // deduplicate
		}
		if u == prev+1 {
			prev = u
			continue
		}
		flush()
		start, prev = u, u
	}
	flush()
	return b.String()
}

// decodeUIDs reads what encodeUIDs wrote.
func decodeUIDs(s string) ([]imapv2.UID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []imapv2.UID
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		a, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("imap: bad uid set %q: %w", s, err)
		}
		b := a
		if isRange {
			b, err = strconv.ParseUint(strings.TrimSpace(hi), 10, 32)
			if err != nil {
				return nil, fmt.Errorf("imap: bad uid set %q: %w", s, err)
			}
		}
		if b < a {
			a, b = b, a
		}
		// A corrupt range must not be allowed to allocate unboundedly.
		if b-a > 5_000_000 {
			return nil, fmt.Errorf("imap: implausible uid range %d:%d", a, b)
		}
		for u := a; u <= b; u++ {
			out = append(out, imapv2.UID(u))
		}
	}
	return out, nil
}

// uidIndex is a set view of an encoded UID list.
func uidIndex(s string) (map[imapv2.UID]bool, error) {
	uids, err := decodeUIDs(s)
	if err != nil {
		return nil, err
	}
	out := make(map[imapv2.UID]bool, len(uids))
	for _, u := range uids {
		out[u] = true
	}
	return out, nil
}
