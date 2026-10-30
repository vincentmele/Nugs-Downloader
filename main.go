package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/alexflint/go-arg"
	"github.com/dustin/go-humanize"
	"github.com/joho/godotenv"
)

const (
	devKey       = "x7f54tgbdyc64y656thy47er4"
	clientId     = "Eg7HuH873H65r5rt325UytR5429"
	layout       = "01/02/2006 15:04:05"
	userAgent    = "NugsNet/3.26.724 (Android; 7.1.2; Asus; ASUS_Z01QD; Scale/2.0; en)"
	userAgentTwo = "nugsnetAndroid"
	authUrl      = "https://id.nugs.net/connect/token"
	subInfoUrl   = "https://subscriptions.nugs.net/api/v1/me/subscriptions"
	userInfoUrl  = "https://id.nugs.net/connect/userinfo"
	playerUrl    = "https://play.nugs.net/"
	configFile   = "config.json"
	envFile      = ".env"
)

// exitInterrupted is the conventional exit status for a process stopped by SIGINT.
const exitInterrupted = 130

const banner = `
 _____                ____                _           _
|   | |_ _ ___ ___   |    \ ___ _ _ _ ___| |___ ___ _| |___ ___
| | | | | | . |_ -|  |  |  | . | | | |   | | . | .'| . | -_|  _|
|_|___|___|_  |___|  |____/|___|_____|_|_|_|___|__,|___|___|_|
          |___|

`

var client = newClient()

// streamApiBase is a variable so tests can point it at a local server.
var streamApiBase = "https://streamapi.nugs.net/"

var (
	sanRegex     = regexp.MustCompile(`[/:*?"><|\x00-\x1f]`)
	durRegex     = regexp.MustCompile(`Duration: ([\d:.]+)`)
	bitrateRegex = regexp.MustCompile(`[\w]+(?:_(\d+)k_v\d+)`)
)

// The index of the first matching pattern selects the media type in run.
var urlRegexes = []*regexp.Regexp{
	regexp.MustCompile(`^https://play\.nugs\.net/release/(\d+)$`),
	regexp.MustCompile(`^https://play\.nugs\.net/#/playlists/playlist/(\d+)$`),
	regexp.MustCompile(`^https://play\.nugs\.net/library/playlist/(\d+)$`),
	regexp.MustCompile(`^(https://2nu\.gs/[a-zA-Z\d]+)$`),
	regexp.MustCompile(`^https://play\.nugs\.net/#/videos/artist/\d+/.+/(\d+)$`),
	regexp.MustCompile(`^https://play\.nugs\.net/(?:browse/)?artist/(\d+)(?:/albums|/latest|)$`),
	regexp.MustCompile(`^https://play\.nugs\.net/livestream/(\d+)/exclusive$`),
	regexp.MustCompile(`^https://play\.nugs\.net/watch/livestreams/exclusive/(\d+)$`),
	regexp.MustCompile(`^https://play\.nugs\.net/#/my-webcasts/\d+-(\d+)-\d+-\d+$`),
	regexp.MustCompile(`^https://www\.nugs\.net/on/demandware\.store/Sites-NugsNet-Site/default/` +
		`(?:Stash-QueueVideo|NugsVideo-GetStashVideo)\?([a-zA-Z0-9=%&-]+)$`),
	regexp.MustCompile(`^https://play\.nugs\.net/library/webcast/(\d+)$`),
	regexp.MustCompile(`^https://play\.nugs\.net/watch/release/(\d+)$`),
}

// The first marker found in a stream URL wins, so HLS manifests come first and
// codec paths come before bare extensions (an ALAC URL also contains ".m4a?").
var streamQualities = []struct {
	marker  string
	quality Quality
}{
	{".m3u8?", Quality{Extension: ".m4a", Format: 6}},
	{".alac16/", Quality{Specs: "16-bit / 44.1 kHz ALAC", Extension: ".m4a", Format: 1}},
	{".flac16/", Quality{Specs: "16-bit / 44.1 kHz FLAC", Extension: ".flac", Format: 2}},
	{".mqa24/", Quality{Specs: "24-bit / 48 kHz MQA", Extension: ".flac", Format: 3}},
	{".s360/", Quality{Specs: "360 Reality Audio", Extension: ".mp4", Format: 4}},
	{".aac150/", Quality{Specs: "150 Kbps AAC", Extension: ".m4a", Format: 5}},
	{".flac?", Quality{Specs: "FLAC", Extension: ".flac", Format: 2}},
	{".m4a?", Quality{Specs: "AAC", Extension: ".m4a", Format: 5}},
}

var resolveRes = map[int]string{
	1: "480",
	2: "720",
	3: "1080",
	4: "1440",
	5: "2160",
}

var trackFallback = map[int]int{
	1: 2,
	2: 5,
	3: 2,
	4: 3,
}

var resFallback = map[string]string{
	"720":  "480",
	"1080": "720",
	"1440": "1080",
}

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	// No overall timeout since downloads can take a long time, but don't wait forever on a
	// server that never answers.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = time.Minute
	return &http.Client{Jar: jar, Transport: t}
}

func newWriteCounter(total int64) *WriteCounter {
	wc := &WriteCounter{Total: total, TotalStr: "?", StartTime: time.Now()}
	if total > 0 {
		wc.TotalStr = humanize.Bytes(uint64(total))
	}
	return wc
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	wc.Downloaded += int64(n)
	var speed float64
	if elapsed := time.Since(wc.StartTime).Seconds(); elapsed > 0 {
		speed = float64(wc.Downloaded) / elapsed
	}
	speedStr := humanize.Bytes(uint64(speed))
	downloadedStr := humanize.Bytes(uint64(wc.Downloaded))
	if wc.Total > 0 {
		fmt.Printf("\r%d%% @ %s/s, %s/%s ", wc.Downloaded*100/wc.Total, speedStr, downloadedStr, wc.TotalStr)
	} else {
		fmt.Printf("\r%s/s, %s ", speedStr, downloadedStr)
	}
	return n, nil
}

func printErr(errText string, err error) {
	fmt.Fprintf(os.Stderr, "%s\n%v\n", errText, err)
}

func wasRunFromSrc() bool {
	buildPath := filepath.Join(os.TempDir(), "go-build")
	return strings.HasPrefix(os.Args[0], buildPath)
}

func getScriptDir() (string, error) {
	var (
		ok    bool
		err   error
		fname string
	)
	if wasRunFromSrc() {
		_, fname, _, ok = runtime.Caller(0)
		if !ok {
			return "", errors.New("failed to get script filename")
		}
	} else {
		fname, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	return filepath.Dir(fname), nil
}

func readTxtFile(path string) ([]string, error) {
	var lines []string
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

func contains(lines []string, value string) bool {
	return slices.ContainsFunc(lines, func(line string) bool {
		return strings.EqualFold(line, value)
	})
}

func processUrls(urls []string) ([]string, error) {
	var processed, txtPaths []string
	add := func(u string) {
		u = strings.TrimSuffix(u, "/")
		if !contains(processed, u) {
			processed = append(processed, u)
		}
	}
	for _, u := range urls {
		if !strings.HasSuffix(u, ".txt") {
			add(u)
			continue
		}
		if contains(txtPaths, u) {
			continue
		}
		txtLines, err := readTxtFile(u)
		if err != nil {
			return nil, err
		}
		for _, txtLine := range txtLines {
			add(txtLine)
		}
		txtPaths = append(txtPaths, u)
	}
	return processed, nil
}

func parseCfg() (*Config, error) {
	cfg, err := readConfig(configFile)
	if err != nil {
		return nil, err
	}
	if err := loadCredentials(cfg, envFile); err != nil {
		return nil, err
	}
	args := parseArgs()
	if args.Format != -1 {
		cfg.Format = args.Format
	}
	if args.VideoFormat != -1 {
		cfg.VideoFormat = args.VideoFormat
	}
	if cfg.Format < 1 || cfg.Format > 5 {
		return nil, errors.New("track format must be between 1 and 5")
	}
	if cfg.VideoFormat < 1 || cfg.VideoFormat > 5 {
		return nil, errors.New("video format must be between 1 and 5")
	}
	cfg.WantRes = resolveRes[cfg.VideoFormat]
	if args.OutPath != "" {
		cfg.OutPath = args.OutPath
	}
	if cfg.OutPath == "" {
		cfg.OutPath = "Nugs downloads"
	}
	cfg.Token = strings.TrimPrefix(cfg.Token, "Bearer ")
	if cfg.UseFfmpegEnvVar {
		cfg.FfmpegNameStr = "ffmpeg"
	} else {
		cfg.FfmpegNameStr = "./ffmpeg"
	}
	cfg.Urls, err = processUrls(args.Urls)
	if err != nil {
		return nil, fmt.Errorf("process URLs: %w", err)
	}
	cfg.ForceVideo = args.ForceVideo
	cfg.SkipVideos = args.SkipVideos
	cfg.SkipChapters = args.SkipChapters
	return cfg, nil
}

func readConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s not found; copy config.example.json to %s and edit it", path, path)
	} else if err != nil {
		return nil, err
	}
	var obj Config
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &obj, nil
}

// loadCredentials fills in the account details from environment variables or the
// .env file at envPath, with real environment variables taking precedence. The
// credentials are treated as one set: if any are found there, any in config.json
// are ignored.
func loadCredentials(cfg *Config, envPath string) error {
	dotenv, err := godotenv.Read(envPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", envPath, err)
	}
	lookup := func(key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return dotenv[key]
	}
	email, password, token := lookup("NUGS_EMAIL"), lookup("NUGS_PASSWORD"), lookup("NUGS_TOKEN")
	if email != "" || password != "" || token != "" {
		cfg.Email, cfg.Password, cfg.Token = email, password, token
		if len(dotenv) > 0 {
			warnIfExposed(envPath)
		}
		return nil
	}
	if cfg.Email != "" || cfg.Password != "" || cfg.Token != "" {
		fmt.Fprintf(os.Stderr,
			"Warning: credentials in %s are deprecated. Move them to %s (see .env.example).\n",
			configFile, envPath)
		warnIfExposed(configFile)
	}
	return nil
}

// warnIfExposed nudges the user when a file holding their credentials is readable
// by other accounts on the machine.
func warnIfExposed(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr,
			"Warning: %s contains credentials and is readable by other users. Run: chmod 600 %s\n",
			path, path)
	}
}

func parseArgs() *Args {
	var args Args
	arg.MustParse(&args)
	return &args
}

func makeDirs(path string) error {
	return os.MkdirAll(path, 0755)
}

func fileExists(path string) (bool, error) {
	f, err := os.Stat(path)
	if err == nil {
		return !f.IsDir(), nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// sanitise makes an API-provided name safe to use as a single path component.
func sanitise(filename string) string {
	san := sanRegex.ReplaceAllString(strings.TrimSuffix(filename, "\t"), "_")
	if os.PathSeparator == '\\' {
		san = strings.ReplaceAll(san, `\`, "_")
	}
	if san == "." || san == ".." {
		return strings.Repeat("_", len(san))
	}
	return san
}

// truncate shortens s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// partPath returns the name a file is written under until it's complete. The
// extension is kept so ffmpeg can still infer the output container.
func partPath(path string) string {
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + ".part" + ext
}

// redactURLError strips the query string, which can carry the user's legacy
// token, from request errors before they're printed.
func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if u, perr := url.Parse(urlErr.URL); perr == nil {
			u.RawQuery = ""
			urlErr.URL = u.String()
		}
	}
	return err
}

func httpGet(ctx context.Context, rawUrl string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawUrl, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// doJSON sends req and decodes a 200 response body into v.
func doJSON(req *http.Request, ua string, v any) error {
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return redactURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func apiGet(ctx context.Context, path string, query url.Values, ua string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamApiBase+path, nil)
	if err != nil {
		return err
	}
	req.URL.RawQuery = query.Encode()
	return doJSON(req, ua, v)
}

func auth(ctx context.Context, email, pwd string) (string, error) {
	data := url.Values{}
	data.Set("client_id", clientId)
	data.Set("grant_type", "password")
	data.Set("scope", "openid profile email nugsnet:api nugsnet:legacyapi offline_access")
	data.Set("username", email)
	data.Set("password", pwd)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authUrl, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var obj Auth
	if err := doJSON(req, userAgent, &obj); err != nil {
		return "", err
	}
	return obj.AccessToken, nil
}

func getUserInfo(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoUrl, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var obj UserInfo
	if err := doJSON(req, userAgent, &obj); err != nil {
		return "", err
	}
	return obj.Sub, nil
}

func getSubInfo(ctx context.Context, token string) (*SubInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, subInfoUrl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var obj SubInfo
	if err := doJSON(req, userAgent, &obj); err != nil {
		return nil, err
	}
	return &obj, nil
}

func getPlan(subInfo *SubInfo) (string, bool) {
	if !reflect.ValueOf(subInfo.Plan).IsZero() {
		return subInfo.Plan.Description, false
	}
	return subInfo.Promo.Plan.Description, true
}

func parseTimestamps(start, end string) (string, string) {
	startTime, _ := time.Parse(layout, start)
	endTime, _ := time.Parse(layout, end)
	parsedStart := strconv.FormatInt(startTime.Unix(), 10)
	parsedEnd := strconv.FormatInt(endTime.Unix(), 10)
	return parsedStart, parsedEnd
}

func parseStreamParams(userId string, subInfo *SubInfo, isPromo bool) *StreamParams {
	startStamp, endStamp := parseTimestamps(subInfo.StartedAt, subInfo.EndsAt)
	streamParams := &StreamParams{
		SubscriptionID:          subInfo.LegacySubscriptionID,
		SubCostplanIDAccessList: subInfo.Plan.PlanID,
		UserID:                  userId,
		StartStamp:              startStamp,
		EndStamp:                endStamp,
	}
	if isPromo {
		streamParams.SubCostplanIDAccessList = subInfo.Promo.Plan.PlanID
	}
	return streamParams
}

func checkUrl(itemUrl string) (string, int) {
	for i, regex := range urlRegexes {
		if match := regex.FindStringSubmatch(itemUrl); match != nil {
			return match[1], i
		}
	}
	return "", 0
}

func extractLegToken(tokenStr string) (string, string, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return "", "", errors.New("token is not a valid JWT")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", err
	}
	var obj Payload
	if err := json.Unmarshal(decoded, &obj); err != nil {
		return "", "", err
	}
	return obj.LegacyToken, obj.LegacyUguid, nil
}

func getAlbumMeta(ctx context.Context, albumId string) (*AlbumMeta, error) {
	query := url.Values{}
	query.Set("method", "catalog.container")
	query.Set("containerID", albumId)
	query.Set("vdisp", "1")
	var obj AlbumMeta
	if err := apiGet(ctx, "api.aspx", query, userAgent, &obj); err != nil {
		return nil, err
	}
	return &obj, nil
}

func getPlistMeta(ctx context.Context, plistId, email, legacyToken string, cat bool) (*PlistMeta, error) {
	path := "secureApi.aspx"
	query := url.Values{}
	if cat {
		path = "api.aspx"
		query.Set("method", "catalog.playlist")
		query.Set("plGUID", plistId)
	} else {
		query.Set("method", "user.playlist")
		query.Set("playlistID", plistId)
		query.Set("developerKey", devKey)
		query.Set("user", email)
		query.Set("token", legacyToken)
	}
	var obj PlistMeta
	if err := apiGet(ctx, path, query, userAgentTwo, &obj); err != nil {
		return nil, err
	}
	return &obj, nil
}

func getArtistMeta(ctx context.Context, artistId string) ([]*ArtistMeta, error) {
	var allArtistMeta []*ArtistMeta
	offset := 1
	query := url.Values{}
	query.Set("method", "catalog.containersAll")
	query.Set("limit", "100")
	query.Set("artistList", artistId)
	query.Set("availType", "1")
	query.Set("vdisp", "1")
	for {
		query.Set("startOffset", strconv.Itoa(offset))
		var obj ArtistMeta
		if err := apiGet(ctx, "api.aspx", query, userAgent, &obj); err != nil {
			return nil, err
		}
		retLen := len(obj.Response.Containers)
		if retLen == 0 {
			break
		}
		allArtistMeta = append(allArtistMeta, &obj)
		offset += retLen
	}
	return allArtistMeta, nil
}

func getPurchasedManUrl(ctx context.Context, skuID int, showID, userID, uguID string) (string, error) {
	query := url.Values{}
	query.Set("skuId", strconv.Itoa(skuID))
	query.Set("showId", showID)
	query.Set("uguid", uguID)
	query.Set("nn_userID", userID)
	query.Set("app", "1")
	var obj PurchasedManResp
	if err := apiGet(ctx, "bigriver/vidPlayer.aspx", query, userAgentTwo, &obj); err != nil {
		return "", err
	}
	return obj.FileURL, nil
}

func getStreamMeta(ctx context.Context, trackId, skuId, format int, streamParams *StreamParams) (string, error) {
	query := url.Values{}
	if format == 0 {
		query.Set("skuId", strconv.Itoa(skuId))
		query.Set("containerID", strconv.Itoa(trackId))
		query.Set("chap", "1")
	} else {
		query.Set("platformID", strconv.Itoa(format))
		query.Set("trackID", strconv.Itoa(trackId))
	}
	query.Set("app", "1")
	query.Set("subscriptionID", streamParams.SubscriptionID)
	query.Set("subCostplanIDAccessList", streamParams.SubCostplanIDAccessList)
	query.Set("nn_userID", streamParams.UserID)
	query.Set("startDateStamp", streamParams.StartStamp)
	query.Set("endDateStamp", streamParams.EndStamp)
	var obj StreamMeta
	if err := apiGet(ctx, "bigriver/subPlayer.aspx", query, userAgentTwo, &obj); err != nil {
		return "", err
	}
	return obj.StreamLink, nil
}

func queryQuality(streamUrl string) *Quality {
	for _, sq := range streamQualities {
		if strings.Contains(streamUrl, sq.marker) {
			q := sq.quality
			q.URL = streamUrl
			return &q
		}
	}
	return nil
}

// fetch streams the body of a track URL into w, printing progress as it goes.
func fetch(ctx context.Context, w io.Writer, trackUrl string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trackUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Referer", playerUrl)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", "bytes=0-")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return errors.New(resp.Status)
	}
	_, err = io.Copy(w, io.TeeReader(resp.Body, newWriteCounter(resp.ContentLength)))
	fmt.Println()
	return err
}

func downloadTrack(ctx context.Context, trackPath, trackUrl string) error {
	f, err := os.OpenFile(trackPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	err = fetch(ctx, f, trackUrl)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func getTrackQual(quals []*Quality, wantFmt int) *Quality {
	for _, quality := range quals {
		if quality.Format == wantFmt {
			return quality
		}
	}
	return nil
}

// pickQuality returns the quality matching wantFmt, following trackFallback when
// it isn't available, along with the format that was chosen.
func pickQuality(quals []*Quality, wantFmt int) (*Quality, int) {
	for f := wantFmt; f != 0; f = trackFallback[f] {
		if q := getTrackQual(quals, f); q != nil {
			return q, f
		}
	}
	return nil, 0
}

func extractBitrate(manUrl string) string {
	if match := bitrateRegex.FindStringSubmatch(manUrl); match != nil {
		return match[1]
	}
	return ""
}

func sortByBandwidth(variants []*m3u8.Variant) {
	slices.SortFunc(variants, func(a, b *m3u8.Variant) int {
		return cmp.Compare(b.Bandwidth, a.Bandwidth)
	})
}

func parseHlsMaster(ctx context.Context, qual *Quality) error {
	master, err := getPlaylist[*m3u8.MasterPlaylist](ctx, qual.URL)
	if err != nil {
		return err
	}
	if len(master.Variants) == 0 {
		return errors.New("master manifest has no variants")
	}
	sortByBandwidth(master.Variants)
	variantUri := master.Variants[0].URI
	bitrate := extractBitrate(variantUri)
	if bitrate == "" {
		return errors.New("no regex match for manifest bitrate")
	}
	qual.Specs = bitrate + " Kbps AAC"
	manBase, q, err := getManifestBase(qual.URL)
	if err != nil {
		return err
	}
	qual.URL = manBase + variantUri + q
	return nil
}

func getKey(ctx context.Context, keyUrl string) ([]byte, error) {
	resp, err := httpGet(ctx, keyUrl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	buf := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// parseIV decodes an EXT-X-KEY IV. When the tag has none, HLS uses the segment's
// media sequence number as a big-endian 128-bit IV.
func parseIV(ivStr string, seqNo uint64) ([]byte, error) {
	if ivStr == "" {
		iv := make([]byte, aes.BlockSize)
		binary.BigEndian.PutUint64(iv[8:], seqNo)
		return iv, nil
	}
	ivHex := strings.TrimPrefix(strings.TrimPrefix(ivStr, "0x"), "0X")
	iv, err := hex.DecodeString(ivHex)
	if err != nil {
		return nil, err
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("IV is %d bytes, want %d", len(iv), aes.BlockSize)
	}
	return iv, nil
}

// pkcs7Unpad strips PKCS#7 padding, returning data unchanged if the padding isn't valid.
func pkcs7Unpad(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return data
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return data
		}
	}
	return data[:len(data)-pad]
}

func decryptTrack(key, iv, encData []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(encData)%aes.BlockSize != 0 {
		return nil, errors.New("encrypted segment isn't a multiple of the AES block size")
	}
	fmt.Println("Decrypting...")
	decrypted := make([]byte, len(encData))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(decrypted, encData)
	return pkcs7Unpad(decrypted), nil
}

func runFfmpeg(cmd *exec.Cmd) error {
	var errBuffer bytes.Buffer
	cmd.Stderr = &errBuffer
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\n%s", err, errBuffer.String())
	}
	return nil
}

func tsToAac(ctx context.Context, decData []byte, outPath, ffmpegNameStr string) error {
	cmd := exec.CommandContext(ctx, ffmpegNameStr, "-hide_banner", "-y", "-i", "pipe:", "-c:a", "copy", outPath)
	cmd.Stdin = bytes.NewReader(decData)
	return runFfmpeg(cmd)
}

func hlsOnly(ctx context.Context, trackPath, manUrl, ffmpegNameStr string) error {
	media, err := getPlaylist[*m3u8.MediaPlaylist](ctx, manUrl)
	if err != nil {
		return err
	}
	segs := media.GetAllSegments()
	if len(segs) == 0 {
		return errors.New("media manifest has no segments")
	}
	if len(media.Keys) == 0 {
		return errors.New("no key found in manifest")
	}
	manBase, q, err := getManifestBase(manUrl)
	if err != nil {
		return err
	}
	key := media.Keys[0]
	keyBytes, err := getKey(ctx, manBase+key.URI)
	if err != nil {
		return fmt.Errorf("get key: %w", err)
	}
	iv, err := parseIV(key.IV, media.SeqNo)
	if err != nil {
		return err
	}
	var encData bytes.Buffer
	if err := fetch(ctx, &encData, manBase+segs[0].URI+q); err != nil {
		return err
	}
	decData, err := decryptTrack(keyBytes, iv, encData.Bytes())
	if err != nil {
		return err
	}
	return tsToAac(ctx, decData, trackPath, ffmpegNameStr)
}

func checkIfHlsOnly(quals []*Quality) bool {
	for _, quality := range quals {
		if !strings.Contains(quality.URL, ".m3u8?") {
			return false
		}
	}
	return true
}

func processTrack(ctx context.Context, folPath string, trackNum, trackTotal int, cfg *Config, track *Track, streamParams *StreamParams) error {
	var quals []*Quality
	// Call the stream meta endpoint four times to get all avail formats since the formats can shift.
	// This will ensure the right format's always chosen.
	for _, i := range [4]int{1, 4, 7, 10} {
		streamUrl, err := getStreamMeta(ctx, track.TrackID, 0, i, streamParams)
		if err != nil {
			return fmt.Errorf("get track stream metadata: %w", err)
		} else if streamUrl == "" {
			return errors.New("the api didn't return a track stream URL")
		}
		quality := queryQuality(streamUrl)
		if quality == nil {
			fmt.Println("The API returned an unsupported format, URL:", streamUrl)
			continue
		}
		quals = append(quals, quality)
	}

	if len(quals) == 0 {
		return errors.New("the api didn't return any formats")
	}

	var chosenQual *Quality
	isHlsOnly := checkIfHlsOnly(quals)
	if isHlsOnly {
		fmt.Println("HLS-only track. Only AAC is available, tags currently unsupported.")
		chosenQual = quals[0]
		if err := parseHlsMaster(ctx, chosenQual); err != nil {
			return fmt.Errorf("parse HLS master manifest: %w", err)
		}
	} else {
		var chosenFmt int
		chosenQual, chosenFmt = pickQuality(quals, cfg.Format)
		if chosenQual == nil {
			return errors.New("no track format was chosen")
		}
		if chosenFmt != cfg.Format && cfg.Format != 4 {
			fmt.Println("Unavailable in your chosen format.")
		}
	}
	// Leave room under the usual 255-byte filename limit for the prefix and extensions.
	title, _ := truncate(sanitise(track.SongTitle), 200)
	trackFname := fmt.Sprintf("%02d. %s%s", trackNum, title, chosenQual.Extension)
	trackPath := filepath.Join(folPath, trackFname)
	exists, err := fileExists(trackPath)
	if err != nil {
		return fmt.Errorf("check if track already exists locally: %w", err)
	}
	if exists {
		fmt.Println("Track already exists locally.")
		return nil
	}
	fmt.Printf(
		"Downloading track %d of %d: %s - %s\n", trackNum, trackTotal, track.SongTitle,
		chosenQual.Specs,
	)
	// Write under a temporary name so an interrupted download is never mistaken
	// for a finished track on the next run.
	tmpPath := partPath(trackPath)
	if isHlsOnly {
		err = hlsOnly(ctx, tmpPath, chosenQual.URL, cfg.FfmpegNameStr)
	} else {
		err = downloadTrack(ctx, tmpPath, chosenQual.URL)
	}
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("download track: %w", err)
	}
	return os.Rename(tmpPath, trackPath)
}

// downloadTracks downloads each track into folPath, reporting failures as it goes.
// It stops early if ctx is cancelled and returns an error if any track failed.
func downloadTracks(ctx context.Context, folPath string, tracks []*Track, cfg *Config, streamParams *StreamParams) error {
	var failed int
	for i, track := range tracks {
		err := processTrack(ctx, folPath, i+1, len(tracks), cfg, track, streamParams)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			printErr("Track failed.", err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d tracks failed", failed, len(tracks))
	}
	return nil
}

func album(ctx context.Context, albumID string, cfg *Config, streamParams *StreamParams, artResp *AlbArtResp) error {
	var (
		meta   *AlbArtResp
		tracks []Track
	)
	if albumID == "" {
		meta = artResp
		tracks = meta.Songs
	} else {
		m, err := getAlbumMeta(ctx, albumID)
		if err != nil {
			return fmt.Errorf("get metadata: %w", err)
		}
		meta = m.Response
		tracks = meta.Tracks
	}

	trackTotal := len(tracks)

	skuID := getVideoSku(meta.Products)

	if skuID == 0 && trackTotal < 1 {
		return errors.New("release has no tracks or videos")
	}

	if skuID != 0 {
		switch {
		case cfg.SkipVideos && trackTotal < 1:
			fmt.Println("Video-only album, skipped.")
			return nil
		case !cfg.SkipVideos && (cfg.ForceVideo || trackTotal < 1):
			return video(ctx, albumID, "", cfg, streamParams, meta, false)
		}
	}
	albumFolder := meta.ArtistName + " - " + strings.TrimRight(meta.ContainerInfo, " ")
	fmt.Println(albumFolder)
	if s, chopped := truncate(albumFolder, 120); chopped {
		albumFolder = s
		fmt.Println(
			"Album folder name was chopped because it exceeds 120 characters.")
	}
	albumPath := filepath.Join(cfg.OutPath, sanitise(albumFolder))
	if err := makeDirs(albumPath); err != nil {
		return fmt.Errorf("make album folder: %w", err)
	}
	trackPtrs := make([]*Track, len(tracks))
	for i := range tracks {
		trackPtrs[i] = &tracks[i]
	}
	return downloadTracks(ctx, albumPath, trackPtrs, cfg, streamParams)
}

func getAlbumTotal(meta []*ArtistMeta) int {
	var total int
	for _, m := range meta {
		total += len(m.Response.Containers)
	}
	return total
}

func artist(ctx context.Context, artistId string, cfg *Config, streamParams *StreamParams) error {
	meta, err := getArtistMeta(ctx, artistId)
	if err != nil {
		return fmt.Errorf("get artist metadata: %w", err)
	}
	if len(meta) == 0 {
		return errors.New("the api didn't return any artist metadata")
	}
	fmt.Println(meta[0].Response.Containers[0].ArtistName)
	albumTotal := getAlbumTotal(meta)
	albumNum, failed := 0, 0
	for _, m := range meta {
		for _, container := range m.Response.Containers {
			albumNum++
			fmt.Printf("Item %d of %d:\n", albumNum, albumTotal)
			if cfg.SkipVideos {
				err = album(ctx, "", cfg, streamParams, container)
			} else {
				// Can't re-use this metadata as it doesn't have any product info for videos.
				err = album(ctx, strconv.Itoa(container.ContainerID), cfg, streamParams, nil)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				printErr("Item failed.", err)
				failed++
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d items failed", failed, albumTotal)
	}
	return nil
}

func playlist(ctx context.Context, plistId, legacyToken string, cfg *Config, streamParams *StreamParams, cat bool) error {
	m, err := getPlistMeta(ctx, plistId, cfg.Email, legacyToken, cat)
	if err != nil {
		return fmt.Errorf("get playlist metadata: %w", err)
	}
	meta := m.Response
	plistName := meta.PlayListName
	fmt.Println(plistName)
	if s, chopped := truncate(plistName, 120); chopped {
		plistName = s
		fmt.Println(
			"Playlist folder name was chopped because it exceeds 120 characters.")
	}
	plistPath := filepath.Join(cfg.OutPath, sanitise(plistName))
	if err := makeDirs(plistPath); err != nil {
		return fmt.Errorf("make playlist folder: %w", err)
	}
	tracks := make([]*Track, len(meta.Items))
	for i := range meta.Items {
		tracks[i] = &meta.Items[i].Track
	}
	return downloadTracks(ctx, plistPath, tracks, cfg, streamParams)
}

func getVideoSku(products []Product) int {
	for _, product := range products {
		formatStr := product.FormatStr
		if formatStr == "VIDEO ON DEMAND" || formatStr == "LIVE HD VIDEO" {
			return product.SkuID
		}
	}
	return 0
}

func getLstreamSku(products []*ProductFormatList) int {
	for _, product := range products {
		if product.FormatStr == "LIVE HD VIDEO" {
			return product.SkuID
		}
	}
	return 0
}

func getVidVariant(variants []*m3u8.Variant, wantRes string) *m3u8.Variant {
	for _, variant := range variants {
		if strings.HasSuffix(variant.Resolution, "x"+wantRes) {
			return variant
		}
	}
	return nil
}

func formatRes(res string) string {
	if res == "2160" {
		return "4K"
	}
	return res + "p"
}

func chooseVariant(ctx context.Context, manifestUrl, wantRes string) (*m3u8.Variant, string, error) {
	master, err := getPlaylist[*m3u8.MasterPlaylist](ctx, manifestUrl)
	if err != nil {
		return nil, "", err
	}
	if len(master.Variants) == 0 {
		return nil, "", errors.New("master manifest has no variants")
	}
	sortByBandwidth(master.Variants)
	if wantRes == "2160" {
		variant := master.Variants[0]
		_, height, ok := strings.Cut(variant.Resolution, "x")
		if !ok {
			return nil, "", fmt.Errorf("unexpected variant resolution %q", variant.Resolution)
		}
		return variant, formatRes(height), nil
	}
	for res := wantRes; res != ""; res = resFallback[res] {
		if variant := getVidVariant(master.Variants, res); variant != nil {
			if res != wantRes {
				fmt.Println("Unavailable in your chosen format.")
			}
			return variant, formatRes(res), nil
		}
	}
	return nil, "", errors.New("no variant was chosen")
}

func getManifestBase(manifestUrl string) (string, string, error) {
	u, err := url.Parse(manifestUrl)
	if err != nil {
		return "", "", err
	}
	path := u.Path
	lastPathIdx := strings.LastIndex(path, "/")
	base := u.Scheme + "://" + u.Host + path[:lastPathIdx+1]
	return base, "?" + u.RawQuery, nil
}

// getPlaylist fetches an HLS manifest and checks that it's the expected kind.
func getPlaylist[T m3u8.Playlist](ctx context.Context, manifestUrl string) (T, error) {
	var zero T
	resp, err := httpGet(ctx, manifestUrl)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return zero, errors.New(resp.Status)
	}
	p, _, err := m3u8.DecodeFrom(resp.Body, true)
	if err != nil {
		return zero, err
	}
	pl, ok := p.(T)
	if !ok {
		return zero, fmt.Errorf("unexpected playlist type %T", p)
	}
	return pl, nil
}

func getSegUrls(ctx context.Context, manifestUrl, query string) ([]string, error) {
	media, err := getPlaylist[*m3u8.MediaPlaylist](ctx, manifestUrl)
	if err != nil {
		return nil, err
	}
	var segUrls []string
	for _, seg := range media.GetAllSegments() {
		segUrls = append(segUrls, seg.URI+query)
	}
	return segUrls, nil
}

// downloadVideo downloads videoUrl to videoPath, resuming a partial download.
// A partial file is kept on failure so the next run can pick up where it left off.
func downloadVideo(ctx context.Context, videoPath, videoUrl string) error {
	f, err := os.OpenFile(videoPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}
	startByte := stat.Size()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, videoUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startByte))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if startByte > 0 {
			fmt.Printf("TS already exists locally, resuming from byte %d...\n", startByte)
		}
	case http.StatusOK:
		// The server ignored the range and is sending the whole file.
		startByte = 0
	case http.StatusRequestedRangeNotSatisfiable:
		if startByte > 0 {
			fmt.Println("TS already fully downloaded.")
			return nil
		}
		return errors.New(resp.Status)
	default:
		return errors.New(resp.Status)
	}
	if err := f.Truncate(startByte); err != nil {
		return err
	}
	if _, err := f.Seek(startByte, io.SeekStart); err != nil {
		return err
	}
	_, err = io.Copy(f, io.TeeReader(resp.Body, newWriteCounter(resp.ContentLength)))
	fmt.Println()
	if err != nil {
		return err
	}
	return f.Close()
}

func copySegment(ctx context.Context, w io.Writer, segUrl string) error {
	resp, err := httpGet(ctx, segUrl)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// downloadLstream concatenates the segments into videoPath. Segmented downloads
// can't be resumed, so a partial file is removed on failure.
func downloadLstream(ctx context.Context, videoPath, baseUrl string, segUrls []string) (err error) {
	f, err := os.OpenFile(videoPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		if err != nil {
			fmt.Println()
			os.Remove(videoPath)
		}
	}()
	segTotal := len(segUrls)
	for segNum, segUrl := range segUrls {
		fmt.Printf("\rSegment %d of %d.", segNum+1, segTotal)
		if err := copySegment(ctx, f, baseUrl+segUrl); err != nil {
			return err
		}
	}
	fmt.Println()
	return f.Close()
}

func extractDuration(errStr string) string {
	if match := durRegex.FindStringSubmatch(errStr); match != nil {
		return match[1]
	}
	return ""
}

func parseDuration(dur string) (int, error) {
	dur = strings.Replace(dur, ":", "h", 1)
	dur = strings.Replace(dur, ":", "m", 1)
	dur = strings.Replace(dur, ".", "s", 1)
	dur += "ms"
	d, err := time.ParseDuration(dur)
	if err != nil {
		return 0, err
	}
	return int(math.Round(d.Seconds())), nil
}

// Horrible, but best way without ffprobe.
// My native Go duration calculation's too slow. Is there a way without having to iterate over all the packets?
func getDuration(ctx context.Context, tsPath, ffmpegNameStr string) (int, error) {
	var errBuffer bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpegNameStr, "-hide_banner", "-nostdin", "-i", tsPath)
	cmd.Stderr = &errBuffer
	// ffmpeg exits non-zero as we're not providing any output files, but it still
	// prints the input's duration.
	err := cmd.Run()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return 0, err
	}
	errStr := errBuffer.String()
	ok := strings.HasSuffix(
		strings.TrimSpace(errStr), "At least one output file must be specified")
	if !ok {
		return 0, fmt.Errorf("%v\n%s", err, errStr)
	}
	dur := extractDuration(errStr)
	if dur == "" {
		return 0, errors.New("no duration found in ffmpeg output")
	}
	return parseDuration(dur)
}

// ffmetaEscaper escapes the characters that are special in ffmpeg's metadata file format.
var ffmetaEscaper = strings.NewReplacer(`\`, `\\`, "=", `\=`, ";", `\;`, "#", `\#`, "\n", "\\\n")

func writeChaps(w io.Writer, chapters []VideoChapter, dur int) error {
	var b strings.Builder
	b.WriteString(";FFMETADATA1\n")
	for i, chapter := range chapters {
		end := dur
		if i+1 < len(chapters) {
			nextChapStart := chapters[i+1].ChapterSeconds
			if nextChapStart <= chapter.ChapterSeconds {
				continue
			}
			end = int(math.Round(nextChapStart) - 1)
		}
		fmt.Fprintf(&b, "\n[CHAPTER]\nTIMEBASE=1/1\nSTART=%d\nEND=%d\nTITLE=%s\n",
			int(math.Round(chapter.ChapterSeconds)), end, ffmetaEscaper.Replace(chapter.ChapterName))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeChapsFile(chapters []VideoChapter, dur int) (string, error) {
	f, err := os.CreateTemp("", "nugs_dl_chapters_*.txt")
	if err != nil {
		return "", err
	}
	err = writeChaps(f, chapters, dur)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// There are native MPEG demuxers and MP4 muxers for Go, but they're too slow.
func tsToMp4(ctx context.Context, vidPathTs, vidPath, ffmpegNameStr, chapsPath string) error {
	args := []string{"-hide_banner", "-nostdin", "-y", "-i", vidPathTs}
	if chapsPath != "" {
		args = append(args, "-f", "ffmetadata", "-i", chapsPath, "-map_metadata", "1")
	}
	args = append(args, "-c", "copy", vidPath)
	return runFfmpeg(exec.CommandContext(ctx, ffmpegNameStr, args...))
}

func video(ctx context.Context, videoID, uguID string, cfg *Config, streamParams *StreamParams, meta *AlbArtResp, isLstream bool) error {
	if meta == nil {
		m, err := getAlbumMeta(ctx, videoID)
		if err != nil {
			return fmt.Errorf("get metadata: %w", err)
		}
		meta = m.Response
	}

	chapsAvail := !cfg.SkipChapters && len(meta.VideoChapters) > 0

	videoFname := meta.ArtistName + " - " + strings.TrimRight(meta.ContainerInfo, " ")
	fmt.Println(videoFname)
	if s, chopped := truncate(videoFname, 110); chopped {
		videoFname = s
		fmt.Println(
			"Video filename was chopped because it exceeds 110 characters.")
	}
	var skuID int
	if isLstream {
		skuID = getLstreamSku(meta.ProductFormatList)
	} else {
		skuID = getVideoSku(meta.Products)
	}
	if skuID == 0 {
		return errors.New("no video available")
	}
	var (
		manifestUrl string
		err         error
	)
	if uguID == "" {
		manifestUrl, err = getStreamMeta(ctx, meta.ContainerID, skuID, 0, streamParams)
	} else {
		manifestUrl, err = getPurchasedManUrl(ctx, skuID, videoID, streamParams.UserID, uguID)
	}
	if err != nil {
		return fmt.Errorf("get video file metadata: %w", err)
	} else if manifestUrl == "" {
		return errors.New("the api didn't return a video manifest url")
	}
	variant, retRes, err := chooseVariant(ctx, manifestUrl, cfg.WantRes)
	if err != nil {
		return fmt.Errorf("get video master manifest: %w", err)
	}
	vidPathNoExt := filepath.Join(cfg.OutPath, sanitise(videoFname+"_"+retRes))
	vidPathTs := vidPathNoExt + ".ts"
	vidPath := vidPathNoExt + ".mp4"
	exists, err := fileExists(vidPath)
	if err != nil {
		return fmt.Errorf("check if video already exists locally: %w", err)
	}
	if exists {
		fmt.Println("Video already exists locally.")
		return nil
	}
	manBaseUrl, query, err := getManifestBase(manifestUrl)
	if err != nil {
		return fmt.Errorf("get video manifest base URL: %w", err)
	}

	segUrls, err := getSegUrls(ctx, manBaseUrl+variant.URI, query)
	if err != nil {
		return fmt.Errorf("get video segment URLs: %w", err)
	}
	if len(segUrls) == 0 {
		return errors.New("video manifest has no segments")
	}

	// Player album page videos aren't always only the first seg for the entire vid.
	isLstream = len(segUrls) > 1 && segUrls[0] != segUrls[1]

	if !isLstream {
		fmt.Printf("%.3f FPS, ", variant.FrameRate)
	}
	fmt.Printf("%d Kbps, %s (%s)\n",
		variant.Bandwidth/1000, retRes, variant.Resolution)
	if isLstream {
		err = downloadLstream(ctx, vidPathTs, manBaseUrl, segUrls)
	} else {
		err = downloadVideo(ctx, vidPathTs, manBaseUrl+segUrls[0])
	}
	if err != nil {
		return fmt.Errorf("download video segments: %w", err)
	}
	var chapsPath string
	if chapsAvail {
		dur, err := getDuration(ctx, vidPathTs, cfg.FfmpegNameStr)
		if err != nil {
			return fmt.Errorf("get TS duration: %w", err)
		}
		chapsPath, err = writeChapsFile(meta.VideoChapters, dur)
		if err != nil {
			return fmt.Errorf("write chapters file: %w", err)
		}
		defer os.Remove(chapsPath)
	}
	fmt.Println("Putting into MP4 container...")
	tmpPath := partPath(vidPath)
	if err := tsToMp4(ctx, vidPathTs, tmpPath, cfg.FfmpegNameStr, chapsPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("put TS into MP4 container: %w", err)
	}
	if err := os.Rename(tmpPath, vidPath); err != nil {
		return err
	}
	if err := os.Remove(vidPathTs); err != nil {
		fmt.Println("Failed to delete TS.")
	}
	return nil
}

func resolveCatPlistId(ctx context.Context, plistUrl string) (string, error) {
	resp, err := httpGet(ctx, plistUrl)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	resolvedId := resp.Request.URL.Query().Get("plGUID")
	if resolvedId == "" {
		return "", errors.New("not a catalog playlist")
	}
	return resolvedId, nil
}

func catalogPlist(ctx context.Context, shortUrl, legacyToken string, cfg *Config, streamParams *StreamParams) error {
	plistId, err := resolveCatPlistId(ctx, shortUrl)
	if err != nil {
		return fmt.Errorf("resolve playlist ID: %w", err)
	}
	return playlist(ctx, plistId, legacyToken, cfg, streamParams, true)
}

func paidLstream(ctx context.Context, query, uguID string, cfg *Config, streamParams *StreamParams) error {
	q, err := url.ParseQuery(query)
	if err != nil {
		return err
	}
	showId := q.Get("showID")
	if showId == "" {
		return errors.New("url didn't contain a show id parameter")
	}
	return video(ctx, showId, uguID, cfg, streamParams, nil, true)
}

func main() {
	os.Exit(run())
}

// run downloads every requested item and returns the process exit status: 0 on
// success, 1 if anything failed, or 130 if interrupted.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Restore the default handling so a second Ctrl+C exits immediately.
		stop()
	}()
	// fail reports a fatal error, or just the interruption if that's what caused it.
	fail := func(errText string, err error) int {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\nInterrupted.")
			return exitInterrupted
		}
		printErr(errText, err)
		return 1
	}

	fmt.Print(banner)
	scriptDir, err := getScriptDir()
	if err != nil {
		return fail("Failed to get script directory.", err)
	}
	if err := os.Chdir(scriptDir); err != nil {
		return fail("Failed to change to script directory.", err)
	}
	cfg, err := parseCfg()
	if err != nil {
		return fail("Failed to parse config/args.", err)
	}
	if err := makeDirs(cfg.OutPath); err != nil {
		return fail("Failed to make output folder.", err)
	}
	token := cfg.Token
	if token == "" {
		token, err = auth(ctx, cfg.Email, cfg.Password)
		if err != nil {
			return fail("Failed to auth.", err)
		}
	}
	userId, err := getUserInfo(ctx, token)
	if err != nil {
		return fail("Failed to get user info.", err)
	}
	subInfo, err := getSubInfo(ctx, token)
	if err != nil {
		return fail("Failed to get subscription info.", err)
	}
	legacyToken, uguID, err := extractLegToken(token)
	if err != nil {
		return fail("Failed to extract legacy token.", err)
	}
	planDesc, isPromo := getPlan(subInfo)
	if !subInfo.IsContentAccessible {
		planDesc = "no active subscription"
	}
	fmt.Println("Signed in successfully - " + planDesc + "\n")
	streamParams := parseStreamParams(userId, subInfo, isPromo)
	itemTotal := len(cfg.Urls)
	failed := 0
	for itemNum, itemUrl := range cfg.Urls {
		if ctx.Err() != nil {
			break
		}
		fmt.Printf("Item %d of %d:\n", itemNum+1, itemTotal)
		itemId, mediaType := checkUrl(itemUrl)
		if itemId == "" {
			fmt.Fprintln(os.Stderr, "Invalid URL:", itemUrl)
			failed++
			continue
		}
		var itemErr error
		switch mediaType {
		case 0:
			itemErr = album(ctx, itemId, cfg, streamParams, nil)
		case 1, 2:
			itemErr = playlist(ctx, itemId, legacyToken, cfg, streamParams, false)
		case 3:
			itemErr = catalogPlist(ctx, itemId, legacyToken, cfg, streamParams)
		case 4, 10, 11:
			itemErr = video(ctx, itemId, "", cfg, streamParams, nil, false)
		case 5:
			itemErr = artist(ctx, itemId, cfg, streamParams)
		case 6, 7, 8:
			itemErr = video(ctx, itemId, "", cfg, streamParams, nil, true)
		case 9:
			itemErr = paidLstream(ctx, itemId, uguID, cfg, streamParams)
		}
		if ctx.Err() != nil {
			break
		}
		if itemErr != nil {
			printErr("Item failed.", itemErr)
			failed++
		}
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "\nInterrupted.")
		return exitInterrupted
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d of %d items failed.\n", failed, itemTotal)
		return 1
	}
	return 0
}
