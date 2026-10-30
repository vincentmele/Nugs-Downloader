package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

var testShow = &AlbArtResp{
	ArtistName:               "Goose",
	ContainerInfo:            "09/02/26 Dillon Amphitheater, Dillon, CO ",
	PerformanceDateFormatted: "2026/09/02",
	ExtImage:                 "goo20260902_cover.jpg",
}

var testAlbum = &AlbArtResp{
	ArtistName:           "Goose",
	ContainerInfo:        "Chateau Sessions Pt. III",
	ReleaseDateFormatted: "2025/05/24",
}

func TestReleasePath(t *testing.T) {
	tests := []struct {
		meta   *AlbArtResp
		format string
		want   string
	}{
		{testShow, "", filepath.Join("out", "Goose", "2026-09-02 Dillon Amphitheater, Dillon, CO")},
		{testAlbum, "", filepath.Join("out", "Goose", "2025-05-24 Chateau Sessions Pt. III")},
		{&AlbArtResp{ArtistName: "Goose", ContainerInfo: "Undated"}, "", filepath.Join("out", "Goose", "Undated")},
		{testShow, "{artist} - {container}", filepath.Join("out", "Goose - 09_02_26 Dillon Amphitheater, Dillon, CO")},
		{testShow, "{date}/{artist}", filepath.Join("out", "2026-09-02", "Goose")},
		{&AlbArtResp{ArtistName: "AC/DC", ContainerInfo: ".."}, "{artist}/{title}", filepath.Join("out", "AC_DC", "__")},
	}
	for _, tt := range tests {
		if got := releasePath("out", tt.format, tt.meta, 120); got != tt.want {
			t.Errorf("releasePath(%q, %q) = %q, want %q", tt.meta.ContainerInfo, tt.format, got, tt.want)
		}
	}
	long := &AlbArtResp{ArtistName: "A", ContainerInfo: strings.Repeat("é", 100)}
	if got := filepath.Base(releasePath("out", "", long, 120)); len(got) > 120 {
		t.Errorf("name is %d bytes, want at most 120", len(got))
	}
}

func TestIsoDate(t *testing.T) {
	for in, want := range map[string]string{"2026/09/02": "2026-09-02", "9/2/2026": "2026-09-02", "09/02/26": "2026-09-02", "": "", "soon": ""} {
		if got := isoDate(in); got != want {
			t.Errorf("isoDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAdoptLegacyPath(t *testing.T) {
	out := t.TempDir()
	legacy := legacyReleasePath(out, testShow, 110) + "_4K.ts"
	if err := os.WriteFile(legacy, []byte("partial video"), 0644); err != nil {
		t.Fatal(err)
	}
	newPath := releasePath(out, "", testShow, 110) + "_4K.ts"
	if err := adoptLegacyPath(legacy, newPath); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(newPath); string(got) != "partial video" {
		t.Errorf("new path holds %q", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("legacy file is still there")
	}
	if want := filepath.Join(out, "Goose", "2026-09-02 Dillon Amphitheater, Dillon, CO_4K.ts"); newPath != want {
		t.Errorf("new path = %q, want %q", newPath, want)
	}
	// Nothing to move, or something already at the new path, is left alone.
	if err := adoptLegacyPath(filepath.Join(out, "missing"), filepath.Join(out, "x")); err != nil {
		t.Error(err)
	}
	os.WriteFile(legacy, []byte("other"), 0644)
	if err := adoptLegacyPath(legacy, newPath); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(newPath); string(got) != "partial video" {
		t.Error("an existing file at the new path was replaced")
	}
}

func TestTrackExtensions(t *testing.T) {
	tests := map[int][]string{
		1: {".m4a", ".flac"},
		2: {".m4a", ".flac"},
		4: {".m4a", ".mp4", ".flac"},
		5: {".m4a"},
	}
	for format, want := range tests {
		if got := trackExtensions(format); !slices.Equal(got, want) {
			t.Errorf("trackExtensions(%d) = %v, want %v", format, got, want)
		}
	}
}

func TestProcessTrackSkipsExistingWithoutAPICalls(t *testing.T) {
	var calls atomic.Int32
	useFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "03. Song 3.flac"), []byte("done"), 0644); err != nil {
		t.Fatal(err)
	}
	track := &Track{TrackID: 3, SongTitle: "Song 3"}
	if err := processTrack(t.Context(), dir, 3, 10, &Config{Format: 4}, track, &StreamParams{}, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Errorf("made %d API calls for a track that's already downloaded", calls.Load())
	}
	// A partial download doesn't count.
	if err := os.WriteFile(filepath.Join(dir, "04. Song 4.part.flac"), []byte("half"), 0644); err != nil {
		t.Fatal(err)
	}
	processTrack(t.Context(), dir, 4, 10, &Config{Format: 2}, &Track{TrackID: 4, SongTitle: "Song 4"}, &StreamParams{}, nil)
	if calls.Load() == 0 {
		t.Error("a partial download was treated as finished")
	}
}

func testJWT(t *testing.T, exp time.Time, legacy string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix(), "legacy_token": legacy, "legacy_uguid": "ug"})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

func TestSessionRefresh(t *testing.T) {
	var refreshes atomic.Int32
	fresh := testJWT(t, time.Now().Add(10*time.Hour), "new-legacy")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "r1" || r.Form.Get("client_id") != clientId {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		refreshes.Add(1)
		json.NewEncoder(w).Encode(Auth{AccessToken: fresh, RefreshToken: "r2"})
	}))
	defer srv.Close()
	orig := authUrl
	authUrl = srv.URL
	defer func() { authUrl = orig }()

	// Far from expiry: nothing happens.
	s, err := newSession(testJWT(t, time.Now().Add(5*time.Hour), "old-legacy"), "r1")
	if err != nil {
		t.Fatal(err)
	}
	s.refreshIfNeeded(t.Context())
	if refreshes.Load() != 0 || s.legacyToken != "old-legacy" {
		t.Fatal("refreshed a login that wasn't close to expiring")
	}
	// About to expire: refreshed, with the new legacy token and refresh token.
	s, _ = newSession(testJWT(t, time.Now().Add(5*time.Minute), "old-legacy"), "r1")
	s.refreshIfNeeded(t.Context())
	if refreshes.Load() != 1 || s.legacyToken != "new-legacy" || s.refreshToken != "r2" || s.token != fresh {
		t.Errorf("after refresh: legacy %q, refresh token %q", s.legacyToken, s.refreshToken)
	}
	// A failed refresh keeps the old login.
	s, _ = newSession(testJWT(t, time.Now().Add(time.Minute), "old-legacy"), "wrong")
	s.refreshIfNeeded(t.Context())
	if s.legacyToken != "old-legacy" {
		t.Error("a failed refresh replaced the login")
	}
	// A token from .env can't be refreshed; it's only reported once expired.
	s, _ = newSession(testJWT(t, time.Now().Add(-time.Minute), "env"), "")
	s.refreshIfNeeded(t.Context())
	if !s.warnedExpired {
		t.Error("no warning for an expired token")
	}
}

func TestSpaceForDownload(t *testing.T) {
	const gb = 1 << 30
	tests := []struct {
		free, estimate, have uint64
		wantErr, wantWarn    bool
	}{
		{100 * gb, 18 * gb, 0, false, false},
		{30 * gb, 18 * gb, 0, false, true},       // can download, but not convert too
		{16 * gb, 18 * gb, 0, false, true},       // within the estimate's margin of error
		{10 * gb, 18 * gb, 0, true, false},       // clearly too little
		{10 * gb, 18 * gb, 17 * gb, false, true}, // nearly downloaded already
	}
	for _, tt := range tests {
		warning, err := spaceForDownload(tt.free, tt.estimate, tt.have)
		if (err != nil) != tt.wantErr || (warning != "") != tt.wantWarn {
			t.Errorf("free %d estimate %d have %d: warning %q, err %v", tt.free/gb, tt.estimate/gb, tt.have/gb, warning, err)
		}
	}
	if free, err := freeSpace(t.TempDir()); err != nil || free == 0 {
		t.Errorf("freeSpace = %d, %v", free, err)
	}
}

func TestRemuxWritesTags(t *testing.T) {
	ts, _ := buildStream(t, streamOpts{audio: true, frames: 30, framesPerAudio: 3, audioStart: 9000})
	cover := append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{1}, 100)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(cover) }))
	defer srv.Close()
	tags := releaseTags(testShow).forTrack("Echo of a Rose", 1, 13)
	tags.art = &coverArt{url: srv.URL}

	f, data := remuxBytes(t, ts, "01. Echo of a Rose.part.m4a", nil, tags)
	var udta *mp4.UdtaBox
	for _, c := range f.Moov.Children {
		if u, ok := c.(*mp4.UdtaBox); ok {
			udta = u
		}
	}
	if udta == nil {
		t.Fatal("no udta box")
	}
	for _, want := range []string{"\xa9nam", "Echo of a Rose", "Goose", "2026-09-02 Dillon Amphitheater, Dillon, CO", "\xa9day", "2026-09-02", "mdirappl"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("tags lack %q", want)
		}
	}
	trkn := []byte("trkn")
	i := bytes.Index(data, trkn)
	if i < 0 {
		t.Fatal("no track number")
	}
	// trkn > data box header (16 bytes) > 2 padding bytes, track, total.
	if track, total := binary.BigEndian.Uint16(data[i+4+16+2:]), binary.BigEndian.Uint16(data[i+4+16+4:]); track != 1 || total != 13 {
		t.Errorf("track number %d of %d, want 1 of 13", track, total)
	}
	covr := bytes.Index(data, []byte("covr"))
	if covr < 0 || !bytes.Contains(data[covr:], cover) || binary.BigEndian.Uint32(data[covr+12:]) != dataJPEG {
		t.Error("cover art missing or not marked as JPEG")
	}
}

func TestWriteCounterThrottles(t *testing.T) {
	var out bytes.Buffer
	wc := newWriteCounter(1 << 20)
	wc.out = &out
	for range 10000 {
		wc.Write(make([]byte, 100))
	}
	wc.finish()
	if n := strings.Count(out.String(), "\r"); n > 5 {
		t.Errorf("redrew progress %d times for a quick download", n)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("%d%%", 10000*100*100/(1<<20))) {
		t.Errorf("final progress missing: %q", out.String())
	}
}
