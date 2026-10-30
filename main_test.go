package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSanitise(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Song", "Song"},
		{`a/b:c*d?e"f<g>h|i`, "a_b_c_d_e_f_g_h_i"},
		{"Trailing tab\t", "Trailing tab"},
		{"new\nline", "new_line"},
		{"..", "__"},
		{".", "_"},
		{"...", "..."},
	}
	for _, tt := range tests {
		if got := sanitise(tt.in); got != tt.want {
			t.Errorf("sanitise(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got, chopped := truncate("short", 10); got != "short" || chopped {
		t.Errorf("truncate(short) = %q, %v", got, chopped)
	}
	// "é" is two bytes; cutting at 4 would split it.
	got, chopped := truncate("abcé", 4)
	if !chopped || got != "abc" || !utf8.ValidString(got) {
		t.Errorf("truncate(abcé, 4) = %q, %v; want \"abc\", true", got, chopped)
	}
}

func TestPartPath(t *testing.T) {
	if got := partPath(filepath.Join("dir", "01. Song.flac")); got != filepath.Join("dir", "01. Song.part.flac") {
		t.Errorf("partPath = %q", got)
	}
}

func TestCheckUrl(t *testing.T) {
	tests := []struct {
		url    string
		wantId string
		wantMT int
	}{
		{"https://play.nugs.net/release/23329", "23329", 0},
		{"https://play.nugs.net/#/playlists/playlist/1215400", "1215400", 1},
		{"https://play.nugs.net/library/playlist/1261211", "1261211", 2},
		{"https://2nu.gs/3PmqXLW", "https://2nu.gs/3PmqXLW", 3},
		{"https://play.nugs.net/#/videos/artist/1045/Dead%20and%20Company/container/27323", "27323", 4},
		{"https://play.nugs.net/artist/461/latest", "461", 5},
		{"https://play.nugs.net/artist/461", "461", 5},
		{"https://play.nugs.net/browse/artist/1205", "1205", 5},
		{"https://play.nugs.net/browse/artist/1205/latest", "1205", 5},
		{"https://play.nugs.net/livestream/30119/exclusive", "30119", 6},
		{"https://play.nugs.net/watch/livestreams/exclusive/30119", "30119", 7},
		{"https://play.nugs.net/#/my-webcasts/5826189-30369-0-624602", "30369", 8},
		{"https://www.nugs.net/on/demandware.store/Sites-NugsNet-Site/default/Stash-QueueVideo?skuID=624598&showID=30367", "skuID=624598&showID=30367", 9},
		{"https://play.nugs.net/library/webcast/123", "123", 10},
		{"https://play.nugs.net/watch/release/49502", "49502", 11},
		// Dots are literal, so look-alike hosts don't match.
		{"https://playXnugs.net/release/23329", "", 0},
		{"https://2nuXgs/3PmqXLW", "", 0},
	}
	for _, tt := range tests {
		id, mt := checkUrl(tt.url)
		if id != tt.wantId || mt != tt.wantMT {
			t.Errorf("checkUrl(%q) = %q, %d; want %q, %d", tt.url, id, mt, tt.wantId, tt.wantMT)
		}
	}
}

func TestQueryQualityIsDeterministic(t *testing.T) {
	tests := []struct {
		url        string
		wantFormat int
	}{
		{"https://cdn/x.alac16/track.m4a?t=1", 1},
		{"https://cdn/x.flac16/track.flac?t=1", 2},
		{"https://cdn/x.mqa24/track.flac?t=1", 3},
		{"https://cdn/x.aac150/track.m4a?t=1", 5},
		{"https://cdn/x.aac150/master.m3u8?t=1", 6},
		{"https://cdn/track.m4a?t=1", 5},
	}
	// The old map-based lookup returned a random match; repeat to catch that.
	for range 50 {
		for _, tt := range tests {
			q := queryQuality(tt.url)
			if q == nil || q.Format != tt.wantFormat || q.URL != tt.url {
				t.Fatalf("queryQuality(%q) = %+v, want format %d", tt.url, q, tt.wantFormat)
			}
		}
	}
	if q := queryQuality("https://cdn/track.ogg"); q != nil {
		t.Errorf("queryQuality(ogg) = %+v, want nil", q)
	}
}

func TestPickQuality(t *testing.T) {
	quals := func(formats ...int) []*Quality {
		var qs []*Quality
		for _, f := range formats {
			qs = append(qs, &Quality{Format: f})
		}
		return qs
	}
	tests := []struct {
		name    string
		have    []*Quality
		want    int
		wantFmt int
	}{
		{"exact", quals(1, 2, 5), 1, 1},
		{"fallback 4 to 3 to 2", quals(2, 5), 4, 2},
		{"fallback 1 to 2 to 5", quals(5), 1, 5},
		// These used to loop forever.
		{"aac unavailable", quals(1, 2), 5, 0},
		{"chain exhausted", quals(1, 6), 2, 0},
	}
	for _, tt := range tests {
		q, f := pickQuality(tt.have, tt.want)
		if f != tt.wantFmt || (tt.wantFmt == 0) != (q == nil) {
			t.Errorf("%s: pickQuality = %+v, %d; want format %d", tt.name, q, f, tt.wantFmt)
		}
	}
}

func TestParseDuration(t *testing.T) {
	got, err := parseDuration("01:02:03.45")
	if err != nil || got != 3723 {
		t.Errorf("parseDuration = %d, %v; want 3723", got, err)
	}
	if _, err := parseDuration("garbage"); err == nil {
		t.Error("parseDuration(garbage) succeeded")
	}
}

func TestParseIV(t *testing.T) {
	iv, err := parseIV("0x000102030405060708090a0b0c0d0e0f", 0)
	if err != nil || iv[0] != 0 || iv[15] != 0x0f {
		t.Errorf("parseIV(hex) = %x, %v", iv, err)
	}
	iv, err = parseIV("", 7)
	if err != nil || iv[15] != 7 || !bytes.Equal(iv[:15], make([]byte, 15)) {
		t.Errorf("parseIV(empty, 7) = %x, %v", iv, err)
	}
	if _, err := parseIV("0x0102", 0); err == nil {
		t.Error("parseIV(short) succeeded")
	}
	if _, err := parseIV("0", 0); err == nil {
		t.Error("parseIV(odd) succeeded")
	}
}

func TestPkcs7Unpad(t *testing.T) {
	padded := append([]byte("hello"), bytes.Repeat([]byte{11}, 11)...)
	if got := pkcs7Unpad(padded); string(got) != "hello" {
		t.Errorf("pkcs7Unpad = %q", got)
	}
	bad := append([]byte("hello world12345"), 3)
	if got := pkcs7Unpad(bad); !bytes.Equal(got, bad) {
		t.Errorf("pkcs7Unpad(invalid) changed the data")
	}
	if got := pkcs7Unpad(nil); got != nil {
		t.Errorf("pkcs7Unpad(nil) = %v", got)
	}
}

func TestDecryptTrackRejectsPartialBlock(t *testing.T) {
	if _, err := decryptTrack(make([]byte, 16), make([]byte, 16), make([]byte, 17)); err == nil {
		t.Error("decryptTrack accepted data that isn't block aligned")
	}
}

func TestWriteChaps(t *testing.T) {
	chapters := []VideoChapter{
		{ChapterSeconds: 0, ChapterName: "Intro"},
		{ChapterSeconds: 0, ChapterName: "Duplicate start"},
		{ChapterSeconds: 61.6, ChapterName: "A=B; #1 \\ ok"},
		{ChapterSeconds: 120, ChapterName: "Encore"},
	}
	var b strings.Builder
	if err := writeChaps(&b, chapters, 300); err != nil {
		t.Fatal(err)
	}
	want := ";FFMETADATA1\n" +
		"\n[CHAPTER]\nTIMEBASE=1/1\nSTART=0\nEND=61\nTITLE=Duplicate start\n" +
		"\n[CHAPTER]\nTIMEBASE=1/1\nSTART=62\nEND=119\nTITLE=A\\=B\\; \\#1 \\\\ ok\n" +
		"\n[CHAPTER]\nTIMEBASE=1/1\nSTART=120\nEND=300\nTITLE=Encore\n"
	if b.String() != want {
		t.Errorf("writeChaps =\n%s\nwant\n%s", b.String(), want)
	}
}

func TestExtractLegToken(t *testing.T) {
	for _, bad := range []string{"", "nodots", "a.b"} {
		if _, _, err := extractLegToken(bad); err == nil {
			t.Errorf("extractLegToken(%q) succeeded", bad)
		}
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"legacy_token":"lt","legacy_uguid":"ug"}`))
	tok, ug, err := extractLegToken("h." + payload + ".s")
	if err != nil || tok != "lt" || ug != "ug" {
		t.Errorf("extractLegToken = %q, %q, %v", tok, ug, err)
	}
}

func TestProcessUrls(t *testing.T) {
	txt := filepath.Join(t.TempDir(), "list.txt")
	content := "https://play.nugs.net/release/1/\n\n  https://play.nugs.net/release/2  \n"
	if err := os.WriteFile(txt, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := processUrls([]string{
		"https://play.nugs.net/release/1",
		txt,
		"https://play.nugs.net/release/2/",
		txt,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://play.nugs.net/release/1", "https://play.nugs.net/release/2"}
	if !slices.Equal(got, want) {
		t.Errorf("processUrls = %q, want %q", got, want)
	}
}

func TestRedactURLError(t *testing.T) {
	_, err := client.Get("http://127.0.0.1:1/secureApi.aspx?token=secret")
	if err == nil {
		t.Skip("expected a connection error")
	}
	if msg := redactURLError(err).Error(); strings.Contains(msg, "secret") {
		t.Errorf("redacted error still contains the token: %s", msg)
	}
}

const testMaster = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=854x480
480/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
1080/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720
720/index.m3u8
`

func serveManifest(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/master.m3u8?auth=x"
}

func TestChooseVariant(t *testing.T) {
	manifest := serveManifest(t, testMaster)
	tests := []struct {
		wantRes, wantURI, wantLabel string
	}{
		{"2160", "1080/index.m3u8", "1080p"},
		{"1080", "1080/index.m3u8", "1080p"},
		{"1440", "1080/index.m3u8", "1080p"},
		{"480", "480/index.m3u8", "480p"},
	}
	for _, tt := range tests {
		v, label, err := chooseVariant(t.Context(), manifest, tt.wantRes)
		if err != nil || v.URI != tt.wantURI || label != tt.wantLabel {
			t.Errorf("chooseVariant(%s) = %v, %q, %v; want %s, %s", tt.wantRes, v, label, err, tt.wantURI, tt.wantLabel)
		}
	}
}

func TestChooseVariantNoMatchReturnsError(t *testing.T) {
	// Used to loop forever once the fallback chain ran out.
	manifest := serveManifest(t, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=640x360\n360.m3u8\n")
	done := make(chan error, 1)
	go func() {
		_, _, err := chooseVariant(t.Context(), manifest, "1080")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("chooseVariant succeeded with no matching variant")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chooseVariant didn't return")
	}
}

func TestChooseVariantRejectsMediaPlaylist(t *testing.T) {
	manifest := serveManifest(t, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nseg.ts\n#EXT-X-ENDLIST\n")
	if _, _, err := chooseVariant(t.Context(), manifest, "1080"); err == nil {
		t.Error("chooseVariant accepted a media playlist")
	}
}

func TestDownloadVideoResumes(t *testing.T) {
	full := bytes.Repeat([]byte("0123456789"), 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "video.ts", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()

	for _, have := range []int{0, 1234, len(full)} {
		path := filepath.Join(t.TempDir(), "video.ts")
		if err := os.WriteFile(path, full[:have], 0644); err != nil {
			t.Fatal(err)
		}
		if err := downloadVideo(t.Context(), path, srv.URL); err != nil {
			t.Fatalf("downloadVideo with %d bytes on disk: %v", have, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, full) {
			t.Errorf("with %d bytes on disk, file has %d bytes and differs from the source", have, len(got))
		}
	}
}

func TestDownloadVideoRestartsWhenRangeIgnored(t *testing.T) {
	full := []byte("the whole file")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(full)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "video.ts")
	if err := os.WriteFile(path, []byte("stale partial data that is longer"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := downloadVideo(t.Context(), path, srv.URL); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, full) {
		t.Errorf("file = %q, want %q", got, full)
	}
}

// unsetEnv removes key for the duration of the test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	os.Unsetenv(key)
}

func TestLoadCredentials(t *testing.T) {
	for _, k := range []string{"NUGS_EMAIL", "NUGS_PASSWORD", "NUGS_TOKEN"} {
		unsetEnv(t, k)
	}
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	missing := filepath.Join(dir, "missing.env")
	dotenv := "# comment\nNUGS_EMAIL=me@example.com\nNUGS_PASSWORD=\"p@ss word#1\"\n"
	if err := os.WriteFile(envPath, []byte(dotenv), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("dotenv replaces config credentials", func(t *testing.T) {
		cfg := &Config{Email: "old", Password: "old", Token: "old"}
		if err := loadCredentials(cfg, envPath); err != nil {
			t.Fatal(err)
		}
		if cfg.Email != "me@example.com" || cfg.Password != "p@ss word#1" || cfg.Token != "" {
			t.Errorf("got %+v", cfg)
		}
	})

	t.Run("environment overrides dotenv", func(t *testing.T) {
		t.Setenv("NUGS_TOKEN", "envtoken")
		cfg := &Config{}
		if err := loadCredentials(cfg, envPath); err != nil {
			t.Fatal(err)
		}
		if cfg.Token != "envtoken" || cfg.Email != "me@example.com" {
			t.Errorf("got %+v", cfg)
		}
	})

	t.Run("config is the fallback", func(t *testing.T) {
		cfg := &Config{Email: "cfg@example.com", Password: "pw"}
		if err := loadCredentials(cfg, missing); err != nil {
			t.Fatal(err)
		}
		if cfg.Email != "cfg@example.com" || cfg.Password != "pw" {
			t.Errorf("got %+v", cfg)
		}
	})
}

func TestConfigExample(t *testing.T) {
	cfg, err := readConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Email != "" || cfg.Password != "" || cfg.Token != "" {
		t.Error("config.example.json shouldn't contain credentials")
	}
	if cfg.Format < 1 || cfg.Format > 5 || cfg.VideoFormat < 1 || cfg.VideoFormat > 5 {
		t.Errorf("config.example.json has invalid formats: %+v", cfg)
	}
	if _, err := readConfig(filepath.Join(t.TempDir(), "config.json")); err == nil ||
		!strings.Contains(err.Error(), "config.example.json") {
		t.Errorf("missing config error = %v, want a hint about config.example.json", err)
	}
}

func TestDownloadVideoInterruptKeepsPartialForResume(t *testing.T) {
	full := bytes.Repeat([]byte("0123456789"), 10000)
	sent := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		w.Write(full[:5000])
		w.(http.Flusher).Flush()
		close(sent)
		<-r.Context().Done()
	}))
	defer stalled.Close()

	path := filepath.Join(t.TempDir(), "video.ts")
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-sent
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := downloadVideo(ctx, path, stalled.URL); err == nil {
		t.Fatal("downloadVideo succeeded despite cancellation")
	}
	partial, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("partial file was removed: %v", err)
	}
	if len(partial) == 0 || !bytes.HasPrefix(full, partial) {
		t.Fatalf("partial file has %d bytes and isn't a prefix of the source", len(partial))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "video.ts", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()
	if err := downloadVideo(t.Context(), path, srv.URL); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, full) {
		t.Errorf("resumed file has %d bytes and differs from the source", len(got))
	}
}

// segmentServer serves numbered segments, failing the ones in failOnce on
// their first request, and counts requests per segment.
type segmentServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests map[string]int
	failOnce map[string]bool
}

func newSegmentServer(t *testing.T, failOnce ...string) *segmentServer {
	t.Helper()
	s := &segmentServer{requests: map[string]int{}, failOnce: map[string]bool{}}
	for _, f := range failOnce {
		s.failOnce[f] = true
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests[r.URL.Path]++
		fail := s.failOnce[r.URL.Path]
		delete(s.failOnce, r.URL.Path)
		s.mu.Unlock()
		if fail {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("<" + r.URL.Path + ">"))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *segmentServer) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.requests {
		n += c
	}
	return n
}

func testSegments(n int, token string) *segmentList {
	segs := &segmentList{duration: float64(n) * 6, target: 6}
	for i := range n {
		segs.urls = append(segs.urls, fmt.Sprintf("seg%d.ts?token=%s", i, token))
	}
	return segs
}

func TestDownloadLstreamResumes(t *testing.T) {
	srv := newSegmentServer(t, "/seg3.ts")
	path := filepath.Join(t.TempDir(), "live.ts")
	want := "</seg0.ts></seg1.ts></seg2.ts></seg3.ts></seg4.ts>"

	if err := downloadLstream(t.Context(), path, srv.URL+"/", testSegments(5, "a")); err == nil {
		t.Fatal("first download succeeded despite a failing segment")
	}
	if got, _ := os.ReadFile(path); string(got) != want[:len("</seg0.ts>")*3] {
		t.Fatalf("partial file = %q", got)
	}
	// The access token changes between runs; the download still resumes.
	if err := downloadLstream(t.Context(), path, srv.URL+"/", testSegments(5, "b")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
	for i := range 3 {
		if n := srv.requests[fmt.Sprintf("/seg%d.ts", i)]; n != 1 {
			t.Errorf("segment %d downloaded %d times, want once", i, n)
		}
	}
	before := srv.total()
	if err := downloadLstream(t.Context(), path, srv.URL+"/", testSegments(5, "c")); err != nil {
		t.Fatal(err)
	}
	if srv.total() != before {
		t.Error("a complete download was downloaded again")
	}
}

func TestDownloadLstreamRestartsForDifferentPlaylist(t *testing.T) {
	srv := newSegmentServer(t)
	path := filepath.Join(t.TempDir(), "live.ts")
	if err := os.WriteFile(path, []byte("old data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeProgress(progressPath(path), lstreamProgress{FirstSegment: "other.ts", Segments: 1, Bytes: 8}); err != nil {
		t.Fatal(err)
	}
	if err := downloadLstream(t.Context(), path, srv.URL+"/", testSegments(2, "a")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "</seg0.ts></seg1.ts>" {
		t.Errorf("file = %q", got)
	}
}

func TestDownloadLstreamRecognizesCompleteFileWithoutProgress(t *testing.T) {
	// A finished download from a version that didn't record progress: about
	// 3 seconds of video.
	ts, _ := buildStream(t, streamOpts{video: true, audio: true, frames: 90, framesPerAudio: 3, videoStart: 900000, audioStart: 900000})
	path := filepath.Join(t.TempDir(), "live.ts")
	if err := os.WriteFile(path, ts, 0644); err != nil {
		t.Fatal(err)
	}
	srv := newSegmentServer(t)
	segs := &segmentList{urls: []string{"seg0.ts", "seg1.ts"}, duration: 3, target: 2}
	if err := downloadLstream(t.Context(), path, srv.URL+"/", segs); err != nil {
		t.Fatal(err)
	}
	if srv.total() != 0 {
		t.Errorf("made %d requests for a complete file", srv.total())
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, ts) {
		t.Error("complete file was modified")
	}

	// The same file is too short for a 60-second playlist, so it's downloaded again.
	other := filepath.Join(t.TempDir(), "live.ts")
	if err := os.WriteFile(other, ts, 0644); err != nil {
		t.Fatal(err)
	}
	segs.duration = 60
	if err := downloadLstream(t.Context(), other, srv.URL+"/", segs); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(other); string(got) != "</seg0.ts></seg1.ts>" {
		t.Errorf("file = %q", got)
	}
}

func TestDownloadVideoCompleteWhenRangeIgnored(t *testing.T) {
	full := bytes.Repeat([]byte("x"), 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ignore the range, announce the whole file, then stall: reading the
		// body would hang the test.
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "video.ts")
	if err := os.WriteFile(path, full, 0644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- downloadVideo(t.Context(), path, srv.URL) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downloadVideo started downloading a complete file again")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, full) {
		t.Error("complete file was modified")
	}
}

func TestDownloadVideoReplacesOversizedFile(t *testing.T) {
	full := []byte("the real video")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "video.ts", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "video.ts")
	if err := os.WriteFile(path, []byte("a different, longer file"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := downloadVideo(t.Context(), path, srv.URL); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, full) {
		t.Errorf("file = %q, want %q", got, full)
	}
}

func TestParseContentRange(t *testing.T) {
	tests := []struct {
		header       string
		first, total int64
	}{
		{"bytes 100-199/1000", 100, 1000},
		{"bytes */1000", -1, 1000},
		{"bytes 0-99/*", 0, -1},
		{"", -1, -1},
	}
	for _, tt := range tests {
		if first, total := parseContentRange(tt.header); first != tt.first || total != tt.total {
			t.Errorf("parseContentRange(%q) = %d, %d; want %d, %d", tt.header, first, total, tt.first, tt.total)
		}
	}
}

func TestTsPresentationSpan(t *testing.T) {
	for _, start := range []int64{900000, tsWrap - 30*testFrameDur} {
		ts, _ := buildStream(t, streamOpts{video: true, audio: true, frames: 90, framesPerAudio: 3, videoStart: start, audioStart: start})
		path := filepath.Join(t.TempDir(), "video.ts")
		if err := os.WriteFile(path, ts, 0644); err != nil {
			t.Fatal(err)
		}
		span, err := tsPresentationSpan(path)
		// 90 frames at 29.97 fps: the last frame starts about 2.97s after the first.
		if err != nil || span < 2900*time.Millisecond || span > 3100*time.Millisecond {
			t.Errorf("start %d: span = %v, %v; want about 2.97s", start, span, err)
		}
	}
}

// useFakeAPI points the stream API at handler for the duration of the test.
func useFakeAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	orig := streamApiBase
	streamApiBase = srv.URL + "/"
	t.Cleanup(func() { streamApiBase = orig })
}

func testTracks(n int) []*Track {
	var tracks []*Track
	for i := range n {
		tracks = append(tracks, &Track{TrackID: i + 1, SongTitle: "Song"})
	}
	return tracks
}

func TestDownloadTracksReportsFailures(t *testing.T) {
	useFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	cfg := &Config{Format: 2}
	err := downloadTracks(t.Context(), t.TempDir(), testTracks(3), cfg, &StreamParams{})
	if err == nil || err.Error() != "3 of 3 tracks failed" {
		t.Errorf("downloadTracks = %v, want 3 of 3 tracks failed", err)
	}
}

func TestDownloadTracksStopsWhenCancelled(t *testing.T) {
	var calls int
	ctx, cancel := context.WithCancel(t.Context())
	useFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		cancel()
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	cfg := &Config{Format: 2}
	err := downloadTracks(ctx, t.TempDir(), testTracks(3), cfg, &StreamParams{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("downloadTracks = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("API called %d times after cancellation, want 1", calls)
	}
}
