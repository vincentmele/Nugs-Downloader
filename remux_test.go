package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/asticode/go-astits"
)

const (
	testVideoPID = 256
	testAudioPID = 257
	testFrameDur = 3003 // 29.97 fps in the 90 kHz clock
	testAudioDur = 1920 // one 1024-sample AAC frame at 48 kHz in the 90 kHz clock
)

// Parameter sets of a 1080p High profile stream as Nugs serves it.
var (
	testSPS = mustHex("27640028ac72301e0089f970152020202800001f4800075307030000802c8000401677bdc07c20104e")
	testPPS = mustHex("28febcb000")
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// testStream builds an MPEG-TS stream in memory.
type testStream struct {
	t   *testing.T
	buf bytes.Buffer
	mux *astits.Muxer
}

func newTestStream(t *testing.T, video, audio bool, videoType astits.StreamType) *testStream {
	t.Helper()
	s := &testStream{t: t}
	s.mux = astits.NewMuxer(context.Background(), &s.buf)
	if video {
		if err := s.mux.AddElementaryStream(astits.PMTElementaryStream{ElementaryPID: testVideoPID, StreamType: videoType}); err != nil {
			t.Fatal(err)
		}
		s.mux.SetPCRPID(testVideoPID)
	}
	if audio {
		if err := s.mux.AddElementaryStream(astits.PMTElementaryStream{ElementaryPID: testAudioPID, StreamType: astits.StreamTypeAACAudio}); err != nil {
			t.Fatal(err)
		}
		if !video {
			s.mux.SetPCRPID(testAudioPID)
		}
	}
	return s
}

func (s *testStream) writePES(pid uint16, streamID uint8, pts, dts int64, data []byte, key bool) {
	s.t.Helper()
	oh := &astits.PESOptionalHeader{
		MarkerBits:      2,
		PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
		PTS:             &astits.ClockReference{Base: pts % tsWrap},
	}
	if dts != pts {
		oh.PTSDTSIndicator = astits.PTSDTSIndicatorBothPresent
		oh.DTS = &astits.ClockReference{Base: dts % tsWrap}
	}
	d := &astits.MuxerData{
		PID: pid,
		PES: &astits.PESData{Header: &astits.PESHeader{StreamID: streamID, OptionalHeader: oh}, Data: data},
	}
	if key {
		d.AdaptationField = &astits.PacketAdaptationField{RandomAccessIndicator: true}
	}
	if _, err := s.mux.WriteData(d); err != nil {
		s.t.Fatal(err)
	}
}

// testAccessUnit returns an Annex B access unit and the length-prefixed MP4
// sample it should become. Payload bytes are never zero, so they can't form a
// start code.
func testAccessUnit(n int, idr bool) (annexB, sample []byte) {
	nalus := [][]byte{{0x09, 0xf0}}
	sliceType := byte(0x01)
	if idr {
		nalus = append(nalus, testSPS, testPPS)
		sliceType = 0x65
	}
	slice := []byte{sliceType}
	for i := range 50 + n%17 {
		slice = append(slice, byte(1+(n*7+i)%255))
	}
	nalus = append(nalus, slice)
	for _, nalu := range nalus {
		annexB = append(annexB, 0, 0, 0, 1)
		annexB = append(annexB, nalu...)
		sample = binary.BigEndian.AppendUint32(sample, uint32(len(nalu)))
		sample = append(sample, nalu...)
	}
	return annexB, sample
}

// testAACFrame returns an ADTS frame and its raw payload.
func testAACFrame(t *testing.T, n int) (adts, payload []byte) {
	t.Helper()
	for i := range 20 + n%11 {
		payload = append(payload, byte(n+i))
	}
	hdr, err := aac.NewADTSHeader(48000, 2, aac.AAClc, uint16(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	return append(hdr.Encode(), payload...), payload
}

type testMedia struct {
	videoSamples [][]byte
	audioSamples [][]byte
}

type streamOpts struct {
	video, audio     bool
	frames           int
	videoStart       int64 // DTS of the first frame
	audioStart       int64 // PTS of the first audio frame
	framesPerAudio   int   // ADTS frames per PES
	jumpAt           int   // video frame after which every timestamp jumps
	jump             int64
	splitAudioFrames bool // end PES packets in the middle of ADTS frames
}

// ctsPattern gives B-frame style composition offsets; the first frame has the
// smallest presentation time.
var ctsPattern = []int64{2 * testFrameDur, 4 * testFrameDur, testFrameDur * 3, testFrameDur * 3}

// buildStream writes video and audio interleaved by time, like a real TS.
func buildStream(t *testing.T, o streamOpts) ([]byte, testMedia) {
	t.Helper()
	s := newTestStream(t, o.video, o.audio, astits.StreamTypeH264Video)
	var media testMedia
	audioFrames := 0
	if o.audio {
		// Cover the same span as the video.
		audioFrames = o.frames * testFrameDur / testAudioDur
	}
	if !o.video {
		o.frames = 0
	}
	v, a := 0, 0
	var pendingAudio []byte
	offset := func(i int, isVideo bool) int64 {
		if o.jumpAt > 0 && ((isVideo && i >= o.jumpAt) || (!isVideo && int64(i)*testAudioDur >= int64(o.jumpAt)*testFrameDur)) {
			return o.jump
		}
		return 0
	}
	for v < o.frames || a < audioFrames {
		videoDTS := o.videoStart + int64(v)*testFrameDur
		audioPTS := o.audioStart + int64(a)*testAudioDur
		if v < o.frames && (a >= audioFrames || videoDTS <= audioPTS) {
			annexB, sample := testAccessUnit(v, v%30 == 0)
			dts := videoDTS + offset(v, true)
			s.writePES(testVideoPID, 0xe0, dts+ctsPattern[v%len(ctsPattern)], dts, annexB, v%30 == 0)
			media.videoSamples = append(media.videoSamples, sample)
			v++
			continue
		}
		pts := audioPTS + offset(a, false)
		data := pendingAudio
		pendingAudio = nil
		for range o.framesPerAudio {
			if a >= audioFrames {
				break
			}
			adts, payload := testAACFrame(t, a)
			data = append(data, adts...)
			media.audioSamples = append(media.audioSamples, payload)
			a++
		}
		if o.splitAudioFrames && a < audioFrames {
			cut := len(data) - 9
			data, pendingAudio = data[:cut], slices.Clone(data[cut:])
		}
		s.writePES(testAudioPID, 0xc0, pts, pts, data, false)
	}
	if len(pendingAudio) > 0 {
		s.writePES(testAudioPID, 0xc0, o.audioStart+int64(a)*testAudioDur, o.audioStart+int64(a)*testAudioDur, pendingAudio, false)
	}
	return s.buf.Bytes(), media
}

func remuxBytes(t *testing.T, ts []byte, name string, chapters []VideoChapter, tags ...*mediaTags) (*mp4.File, []byte) {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	var tg *mediaTags
	if len(tags) > 0 {
		tg = tags[0]
	}
	if err := remuxToMp4(t.Context(), bytes.NewReader(ts), out, chapters, tg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output doesn't parse as MP4: %v", err)
	}
	return f, data
}

// trakSamples reads every sample of a track from the file.
func trakSamples(t *testing.T, trak *mp4.TrakBox, file []byte) [][]byte {
	t.Helper()
	stbl := trak.Mdia.Minf.Stbl
	var offsets []uint64
	if stbl.Co64 != nil {
		offsets = stbl.Co64.ChunkOffset
	} else {
		for _, o := range stbl.Stco.ChunkOffset {
			offsets = append(offsets, uint64(o))
		}
	}
	var samples [][]byte
	nr := uint32(1)
	for i, e := range stbl.Stsc.Entries {
		last := uint32(len(offsets))
		if i+1 < len(stbl.Stsc.Entries) {
			last = stbl.Stsc.Entries[i+1].FirstChunk - 1
		}
		for chunk := e.FirstChunk; chunk <= last; chunk++ {
			pos := offsets[chunk-1]
			for range e.SamplesPerChunk {
				size := uint64(stbl.Stsz.GetSampleSize(int(nr)))
				samples = append(samples, file[pos:pos+size])
				pos += size
				nr++
			}
		}
	}
	return samples
}

func sttsDurations(stts *mp4.SttsBox) []uint32 {
	var d []uint32
	for i, c := range stts.SampleCount {
		for range c {
			d = append(d, stts.SampleTimeDelta[i])
		}
	}
	return d
}

func allEqual(d []uint32, want uint32) bool {
	return !slices.ContainsFunc(d, func(x uint32) bool { return x != want })
}

func TestRemuxVideoAndAudio(t *testing.T) {
	ts, media := buildStream(t, streamOpts{
		video: true, audio: true, frames: 90, framesPerAudio: 3,
		videoStart: 900000, audioStart: 900000 + 2*testFrameDur + 1800,
	})
	f, data := remuxBytes(t, ts, "out.mp4", nil)

	if f.Ftyp.MajorBrand() != "isom" {
		t.Errorf("major brand = %q", f.Ftyp.MajorBrand())
	}
	if len(f.Moov.Traks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(f.Moov.Traks))
	}
	video, audio := f.Moov.Traks[0], f.Moov.Traks[1]

	if h := video.Mdia.Hdlr.HandlerType; h != "vide" {
		t.Errorf("track 1 handler = %q", h)
	}
	vstbl := video.Mdia.Minf.Stbl
	avc1 := vstbl.Stsd.AvcX
	if avc1 == nil || avc1.Width != 1920 || avc1.Height != 1080 {
		t.Fatalf("video sample entry = %+v", avc1)
	}
	if !bytes.Equal(avc1.AvcC.SPSnalus[0], testSPS) || !bytes.Equal(avc1.AvcC.PPSnalus[0], testPPS) {
		t.Error("avcC parameter sets differ from the stream's")
	}
	if video.Tkhd.Width != 1920<<16 || video.Tkhd.Height != 1080<<16 {
		t.Errorf("tkhd size = %v x %v", video.Tkhd.Width, video.Tkhd.Height)
	}
	if got := trakSamples(t, video, data); !slices.EqualFunc(got, media.videoSamples, bytes.Equal) {
		t.Error("video samples differ from the stream's access units")
	}
	if d := sttsDurations(vstbl.Stts); len(d) != 90 || !allEqual(d, testFrameDur) {
		t.Errorf("video durations = %v", d)
	}
	if !slices.Equal(vstbl.Stss.SampleNumber, []uint32{1, 31, 61}) {
		t.Errorf("sync samples = %v", vstbl.Stss.SampleNumber)
	}
	if vstbl.Ctts == nil || vstbl.Ctts.GetCompositionTimeOffset(1) != 2*testFrameDur ||
		vstbl.Ctts.GetCompositionTimeOffset(2) != 4*testFrameDur {
		t.Error("composition offsets weren't carried over")
	}
	// Video presents first: no delay, but skip the B-frame decode delay.
	if e := video.Edts.Elst[0].Entries; len(e) != 1 || e[0].MediaTime != 2*testFrameDur {
		t.Errorf("video edit list = %+v", e)
	}

	astbl := audio.Mdia.Minf.Stbl
	mp4a := astbl.Stsd.Mp4a
	if mp4a == nil || mp4a.SampleRate != 48000 || mp4a.ChannelCount != 2 {
		t.Fatalf("audio sample entry = %+v", mp4a)
	}
	if asc := mp4a.Esds.DecConfigDescriptor.DecSpecificInfo.DecConfig; !bytes.Equal(asc, []byte{0x11, 0x90}) {
		t.Errorf("AudioSpecificConfig = %x, want 1190", asc)
	}
	if audio.Mdia.Mdhd.Timescale != 48000 {
		t.Errorf("audio timescale = %d", audio.Mdia.Mdhd.Timescale)
	}
	if got := trakSamples(t, audio, data); !slices.EqualFunc(got, media.audioSamples, bytes.Equal) {
		t.Errorf("audio samples differ (%d vs %d)", len(got), len(media.audioSamples))
	}
	if d := sttsDurations(astbl.Stts); !allEqual(d, aacFrameSamples) {
		t.Errorf("audio durations = %v", d)
	}
	// Audio starts 20 ms after the video.
	if e := audio.Edts.Elst[0].Entries; len(e) != 2 || e[0].MediaTime != -1 || e[0].SegmentDuration != 20 || e[1].MediaTime != 0 {
		t.Errorf("audio edit list = %+v", e)
	}
	if f.Moov.Mvhd.Duration == 0 || f.Moov.Mvhd.Timescale != movieTimescale {
		t.Errorf("mvhd = %+v", f.Moov.Mvhd)
	}
	if f.Moov.Mvhd.Duration < 3000 || f.Moov.Mvhd.Duration > 3100 {
		t.Errorf("movie duration = %d ms, want about 3003", f.Moov.Mvhd.Duration)
	}
}

func TestRemuxAudioOnly(t *testing.T) {
	ts, media := buildStream(t, streamOpts{audio: true, frames: 60, framesPerAudio: 5, audioStart: 123456})
	f, data := remuxBytes(t, ts, "track.part.m4a", nil)
	if f.Ftyp.MajorBrand() != "M4A " {
		t.Errorf("major brand = %q", f.Ftyp.MajorBrand())
	}
	if len(f.Moov.Traks) != 1 {
		t.Fatalf("got %d tracks", len(f.Moov.Traks))
	}
	audio := f.Moov.Traks[0]
	if got := trakSamples(t, audio, data); !slices.EqualFunc(got, media.audioSamples, bytes.Equal) {
		t.Error("audio samples differ")
	}
	if e := audio.Edts.Elst[0].Entries; len(e) != 1 || e[0].MediaTime != 0 {
		t.Errorf("edit list = %+v", e)
	}
}

func TestRemuxSplitADTSFrames(t *testing.T) {
	ts, media := buildStream(t, streamOpts{audio: true, frames: 60, framesPerAudio: 4, audioStart: 5000, splitAudioFrames: true})
	f, data := remuxBytes(t, ts, "track.m4a", nil)
	audio := f.Moov.Traks[0]
	if got := trakSamples(t, audio, data); !slices.EqualFunc(got, media.audioSamples, bytes.Equal) {
		t.Errorf("audio samples differ (%d vs %d)", len(got), len(media.audioSamples))
	}
	if d := sttsDurations(audio.Mdia.Minf.Stbl.Stts); !allEqual(d, aacFrameSamples) {
		t.Errorf("audio durations = %v", d)
	}
}

func TestRemuxTimestampWraparound(t *testing.T) {
	start := tsWrap - 20*testFrameDur
	ts, _ := buildStream(t, streamOpts{
		video: true, audio: true, frames: 60, framesPerAudio: 3,
		videoStart: start, audioStart: start + 2*testFrameDur,
	})
	f, _ := remuxBytes(t, ts, "out.mp4", nil)
	if d := sttsDurations(f.Moov.Traks[0].Mdia.Minf.Stbl.Stts); len(d) != 60 || !allEqual(d, testFrameDur) {
		t.Errorf("video durations across the wrap = %v", d)
	}
	if d := sttsDurations(f.Moov.Traks[1].Mdia.Minf.Stbl.Stts); !allEqual(d, aacFrameSamples) {
		t.Errorf("audio durations across the wrap = %v", d)
	}
}

func TestRemuxDiscontinuity(t *testing.T) {
	ts, media := buildStream(t, streamOpts{
		video: true, audio: true, frames: 90, framesPerAudio: 3,
		videoStart: 900000, audioStart: 900000 + 2*testFrameDur,
		jumpAt: 45, jump: -3600 * tsClockRate,
	})
	f, _ := remuxBytes(t, ts, "out.mp4", nil)
	video, audio := f.Moov.Traks[0], f.Moov.Traks[1]
	if d := sttsDurations(video.Mdia.Minf.Stbl.Stts); len(d) != 90 || !allEqual(d, testFrameDur) {
		t.Errorf("video durations across the discontinuity = %v", d)
	}
	ad := sttsDurations(audio.Mdia.Minf.Stbl.Stts)
	if len(ad) != len(media.audioSamples) {
		t.Fatalf("got %d audio samples, want %d", len(ad), len(media.audioSamples))
	}
	// Tracks stay within a frame of each other instead of drifting an hour apart.
	vDur := float64(video.Mdia.Mdhd.Duration) / tsClockRate
	aDur := float64(audio.Mdia.Mdhd.Duration) / 48000
	if diff := vDur - aDur; diff > 0.1 || diff < -0.1 {
		t.Errorf("video lasts %.3fs, audio %.3fs", vDur, aDur)
	}
}

func TestRemuxChapters(t *testing.T) {
	ts, _ := buildStream(t, streamOpts{
		video: true, audio: true, frames: 90, framesPerAudio: 3,
		videoStart: 900000, audioStart: 900000 + 2*testFrameDur,
	})
	chapters := []VideoChapter{
		{ChapterSeconds: 1.5, ChapterName: "Song A=B; #1"},
		{ChapterSeconds: 0, ChapterName: "Intro"},
		{ChapterSeconds: 0, ChapterName: "Set 1"},
		{ChapterSeconds: 100, ChapterName: "After the end"},
	}
	f, data := remuxBytes(t, ts, "out.mp4", chapters)
	if len(f.Moov.Traks) != 3 {
		t.Fatalf("got %d tracks, want video, audio and chapters", len(f.Moov.Traks))
	}
	chap := f.Moov.Traks[2]
	if chap.Mdia.Hdlr.HandlerType != "text" || chap.Tkhd.Flags != 0x2 {
		t.Errorf("chapter track handler %q flags %#x", chap.Mdia.Hdlr.HandlerType, chap.Tkhd.Flags)
	}
	samples := trakSamples(t, chap, data)
	wantTitles := []string{"Set 1", "Song A=B; #1"}
	if len(samples) != len(wantTitles) {
		t.Fatalf("got %d chapter samples, want %d", len(samples), len(wantTitles))
	}
	for i, s := range samples {
		n := binary.BigEndian.Uint16(s)
		if got := string(s[2 : 2+n]); got != wantTitles[i] {
			t.Errorf("chapter %d title = %q, want %q", i, got, wantTitles[i])
		}
		if !bytes.HasSuffix(s, []byte("encd\x00\x00\x01\x00")) {
			t.Errorf("chapter %d lacks the encd box", i)
		}
	}
	movieDur := uint32(f.Moov.Mvhd.Duration)
	if d := sttsDurations(chap.Mdia.Minf.Stbl.Stts); !slices.Equal(d, []uint32{1500, movieDur - 1500}) {
		t.Errorf("chapter durations = %v, want [1500 %d]", d, movieDur-1500)
	}
	for _, trak := range f.Moov.Traks[:2] {
		var refs []uint32
		for _, c := range trak.Children {
			if tref, ok := c.(*mp4.TrefBox); ok {
				// mp4ff decodes chap references as unknown boxes holding the track IDs.
				for _, ref := range tref.Children {
					if u, ok := ref.(*mp4.UnknownBox); ok && u.Type() == "chap" {
						for p := u.Payload(); len(p) >= 4; p = p[4:] {
							refs = append(refs, binary.BigEndian.Uint32(p))
						}
					}
				}
			}
		}
		if !slices.Equal(refs, []uint32{3}) {
			t.Errorf("track %d chapter reference = %v", trak.Tkhd.TrackID, refs)
		}
	}
	// The Nero chapter box: version 1, then the count and each start in 100ns units.
	wantChpl := []byte{1, 0, 0, 0, 0, 0, 0, 0, 2}
	wantChpl = binary.BigEndian.AppendUint64(wantChpl, 0)
	wantChpl = append(wantChpl, 5)
	wantChpl = append(wantChpl, "Set 1"...)
	wantChpl = binary.BigEndian.AppendUint64(wantChpl, 15000000)
	wantChpl = append(wantChpl, 12)
	wantChpl = append(wantChpl, "Song A=B; #1"...)
	if !bytes.Contains(data, append([]byte("chpl"), wantChpl...)) {
		t.Error("chpl box missing or wrong")
	}
}

func TestRemuxRejectsHEVC(t *testing.T) {
	s := newTestStream(t, true, false, astits.StreamTypeH265Video)
	s.writePES(testVideoPID, 0xe0, 1000, 1000, []byte{0, 0, 0, 1, 0x40, 0x01}, true)
	err := remuxToMp4(t.Context(), bytes.NewReader(s.buf.Bytes()), filepath.Join(t.TempDir(), "out.mp4"), nil, nil)
	if !errors.Is(err, errUnsupportedCodec) {
		t.Errorf("err = %v, want errUnsupportedCodec", err)
	}
}

func TestRemuxEmptyStream(t *testing.T) {
	err := remuxToMp4(t.Context(), bytes.NewReader(nil), filepath.Join(t.TempDir(), "out.mp4"), nil, nil)
	if err == nil {
		t.Error("remuxing an empty stream succeeded")
	}
}

func TestRemuxCancelled(t *testing.T) {
	ts, _ := buildStream(t, streamOpts{video: true, frames: 30, videoStart: 1000})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := remuxToMp4(ctx, bytes.NewReader(ts), filepath.Join(t.TempDir(), "out.mp4"), nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestChunkOffsetBox(t *testing.T) {
	if _, ok := chunkOffsetBox([]uint64{100, 1 << 31}).(*mp4.StcoBox); !ok {
		t.Error("offsets under 4 GiB should use stco")
	}
	if _, ok := chunkOffsetBox([]uint64{100, 1 << 33}).(*mp4.Co64Box); !ok {
		t.Error("offsets over 4 GiB should use co64")
	}
}

func TestHlsOnlyWithoutFfmpeg(t *testing.T) {
	ts, media := buildStream(t, streamOpts{audio: true, frames: 60, framesPerAudio: 5, audioStart: 9000})
	key := bytes.Repeat([]byte{7}, 16)
	iv := mustHex("000102030405060708090a0b0c0d0e0f")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(ts)%aes.BlockSize
	enc := append(slices.Clone(ts), bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, enc)
	playlist := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x000102030405060708090a0b0c0d0e0f\n" +
		"#EXTINF:2.0,\nseg0.ts\n#EXT-X-ENDLIST\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hls/index.m3u8":
			w.Write([]byte(playlist))
		case "/hls/key.bin":
			w.Write(key)
		case "/hls/seg0.ts":
			w.Write(enc)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "01. Song.part.m4a")
	if err := hlsOnly(t.Context(), out, srv.URL+"/hls/index.m3u8?token=x", &Config{}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if f.Ftyp.MajorBrand() != "M4A " || len(f.Moov.Traks) != 1 {
		t.Fatalf("brand %q with %d tracks", f.Ftyp.MajorBrand(), len(f.Moov.Traks))
	}
	if got := trakSamples(t, f.Moov.Traks[0], data); !slices.EqualFunc(got, media.audioSamples, bytes.Equal) {
		t.Errorf("audio samples differ (%d vs %d)", len(got), len(media.audioSamples))
	}
}
