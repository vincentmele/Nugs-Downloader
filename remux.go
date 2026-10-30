package main

// Native MPEG-TS to MP4 remuxing, replacing ffmpeg's "-c copy". Audio and video
// are repackaged without re-encoding, laid out the way ffmpeg's mp4 muxer does:
// one mdat followed by the moov, edit lists to keep audio and video in sync,
// and chapters as both a Nero chpl box and a QuickTime chapter text track.

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/asticode/go-astits"
)

const (
	movieTimescale = 1000
	tsClockRate    = 90000
	tsWrap         = int64(1) << 33
	// Timestamp jumps bigger than this are treated as stream discontinuities,
	// matching ffmpeg's default -dts_delta_threshold.
	maxTsJump         = 10 * tsClockRate
	aacFrameSamples   = 1024
	defaultFrameDur90 = 3003
	mdatHeaderSize    = 16
)

var errUnsupportedCodec = errors.New("unsupported codec")

type remuxTrack struct {
	video     bool
	pid       uint16
	trackID   uint32
	timescale uint32

	// The sample table in decode order, with timestamps in the track timescale.
	sizes      []uint32
	dts        []int64
	ctsOffsets []int32
	syncs      []uint32 // 1-based numbers of sync samples, video only
	chunks     []remuxChunk
	minPTS     int64

	// Codec configuration, taken from the first samples that carry it.
	sps, pps   [][]byte
	sampleRate int
	channels   int
	objectType byte

	// Timestamp continuity, in the 90 kHz TS clock.
	hasLast    bool
	lastRaw    int64
	wrapOffset int64
	last90     int64
	frameDur90 int64

	// Audio framing state.
	pending      []byte
	nextAudioDTS int64
}

type remuxChunk struct {
	offset  uint64
	samples uint32
}

type remuxer struct {
	f          *os.File
	w          *bufio.Writer
	pos        uint64
	mdatStart  uint64
	video      *remuxTrack
	audio      *remuxTrack
	byPID      map[uint16]*remuxTrack
	lastTrack  *remuxTrack
	discOffset int64 // shift applied to every track after a discontinuity
}

// remuxFile repackages the MPEG-TS file at inPath into an MP4 file at outPath.
func remuxFile(ctx context.Context, inPath, outPath string, chapters []VideoChapter) error {
	f, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return remuxToMp4(ctx, f, outPath, chapters)
}

// remuxToMp4 repackages the H.264 video and AAC audio of the MPEG-TS stream r
// into an MP4 file at outPath, adding chapters if given. An outPath ending in
// .m4a gets an audio file brand.
func remuxToMp4(ctx context.Context, r io.Reader, outPath string, chapters []VideoChapter) (err error) {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	rm := &remuxer{f: f, w: bufio.NewWriterSize(f, 4<<20), byPID: map[uint16]*remuxTrack{}}
	if err := rm.writeHeader(filepath.Ext(outPath) == ".m4a"); err != nil {
		return err
	}
	if err := rm.demux(ctx, r); err != nil {
		return err
	}
	return rm.finish(chapters)
}

func (rm *remuxer) write(p []byte) error {
	n, err := rm.w.Write(p)
	rm.pos += uint64(n)
	return err
}

func (rm *remuxer) writeHeader(audioOnly bool) error {
	ftyp := mp4.NewFtyp("isom", 0x200, []string{"isom", "iso2", "avc1", "mp41"})
	if audioOnly {
		ftyp = mp4.NewFtyp("M4A ", 0x200, []string{"M4A ", "isom", "iso2", "mp41"})
	}
	var buf bytes.Buffer
	if err := ftyp.Encode(&buf); err != nil {
		return err
	}
	if err := rm.write(buf.Bytes()); err != nil {
		return err
	}
	// The mdat uses a 64-bit size, filled in once all samples are written.
	rm.mdatStart = rm.pos
	hdr := make([]byte, mdatHeaderSize)
	binary.BigEndian.PutUint32(hdr, 1)
	copy(hdr[4:], "mdat")
	return rm.write(hdr)
}

func (rm *remuxer) demux(ctx context.Context, r io.Reader) error {
	dmx := astits.NewDemuxer(ctx, bufio.NewReaderSize(r, 1<<20))
	for {
		d, err := dmx.NextData()
		if errors.Is(err, astits.ErrNoMorePackets) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("demux: %w", err)
		}
		switch {
		case d.PMT != nil:
			if err := rm.addStreams(d.PMT); err != nil {
				return err
			}
		case d.PES != nil:
			t := rm.byPID[d.PID]
			if t == nil {
				continue
			}
			if t.video {
				err = rm.addVideo(t, d.PES)
			} else {
				err = rm.addAudio(t, d.PES)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (rm *remuxer) addStreams(pmt *astits.PMTData) error {
	for _, es := range pmt.ElementaryStreams {
		if rm.byPID[es.ElementaryPID] != nil {
			continue
		}
		switch es.StreamType {
		case astits.StreamTypeH264Video:
			if rm.video == nil {
				rm.video = &remuxTrack{video: true, pid: es.ElementaryPID, timescale: tsClockRate, frameDur90: defaultFrameDur90}
				rm.byPID[es.ElementaryPID] = rm.video
			}
		case astits.StreamTypeAACAudio:
			if rm.audio == nil {
				rm.audio = &remuxTrack{pid: es.ElementaryPID}
				rm.byPID[es.ElementaryPID] = rm.audio
			}
		case astits.StreamTypeH265Video:
			return fmt.Errorf("%w: HEVC video (try --ffmpeg)", errUnsupportedCodec)
		}
	}
	return nil
}

func pesTimestamps(pes *astits.PESData) (pts, dts int64, ok bool) {
	oh := pes.Header.OptionalHeader
	if oh == nil || oh.PTS == nil {
		return 0, 0, false
	}
	pts = oh.PTS.Base
	dts = pts
	if oh.DTS != nil {
		dts = oh.DTS.Base
	}
	return pts, dts, true
}

// normalize maps a raw 33-bit TS timestamp onto one continuous timeline: it
// undoes wraparound per track and, like ffmpeg, closes discontinuities by
// shifting every track by the same amount.
func (rm *remuxer) normalize(t *remuxTrack, raw int64) int64 {
	ts := raw + t.wrapOffset
	if t.hasLast && ts < t.lastRaw-tsWrap/2 {
		t.wrapOffset += tsWrap
		ts += tsWrap
	}
	t.lastRaw = ts
	ts += rm.discOffset
	if t.hasLast {
		if d := ts - t.last90; d < -maxTsJump || d > maxTsJump {
			want := t.last90 + t.frameDur90
			rm.discOffset += want - ts
			ts = want
		}
	}
	return ts
}

func (rm *remuxer) addVideo(t *remuxTrack, pes *astits.PESData) error {
	rawPTS, rawDTS, ok := pesTimestamps(pes)
	if !ok || len(pes.Data) == 0 {
		return nil
	}
	ctsOffset := rawPTS - rawDTS
	if ctsOffset < -tsWrap/2 {
		ctsOffset += tsWrap
	} else if ctsOffset > tsWrap/2 {
		ctsOffset -= tsWrap
	}
	dts := rm.normalize(t, rawDTS)
	if n := len(t.dts); n > 0 {
		if dts <= t.dts[n-1] {
			dts = t.dts[n-1] + 1
		} else {
			t.frameDur90 = dts - t.dts[n-1]
		}
	}
	t.last90, t.hasLast = dts, true

	sample := avc.ConvertByteStreamToNaluSample(pes.Data)
	if t.sps == nil {
		if sps, pps := avc.GetParameterSets(sample); len(sps) > 0 && len(pps) > 0 {
			t.sps, t.pps = cloneAll(sps), cloneAll(pps)
		}
	}
	pts := dts + ctsOffset
	if len(t.dts) == 0 || pts < t.minPTS {
		t.minPTS = pts
	}
	if avc.IsIDRSample(sample) {
		t.syncs = append(t.syncs, uint32(len(t.sizes)+1))
	}
	t.ctsOffsets = append(t.ctsOffsets, int32(ctsOffset))
	return rm.writeSample(t, sample, dts)
}

func (rm *remuxer) addAudio(t *remuxTrack, pes *astits.PESData) error {
	rawPTS, _, hasPTS := pesTimestamps(pes)
	var pesTS int64
	if hasPTS {
		pts90 := rm.normalize(t, rawPTS)
		t.last90, t.hasLast = pts90, true
		if t.sampleRate > 0 {
			pesTS = pts90 * int64(t.sampleRate) / tsClockRate
		}
	}
	data := pes.Data
	carried := len(t.pending)
	if carried > 0 {
		data = append(t.pending, data...)
		t.pending = nil
	}
	timestamped := !hasPTS
	for len(data) > 0 {
		hdr, skip, err := aac.DecodeADTSHeader(bytes.NewReader(data))
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				t.pending = slices.Clone(data)
			}
			// Otherwise the rest of this PES isn't ADTS; skip it and resync on the next one.
			return nil
		}
		frameLen := int(hdr.HeaderLength) + int(hdr.PayloadLength)
		if skip+frameLen > len(data) {
			t.pending = slices.Clone(data[skip:])
			return nil
		}
		if t.sampleRate == 0 {
			t.sampleRate = int(hdr.Frequency())
			t.timescale = uint32(t.sampleRate)
			t.channels = aac.NrChannels(hdr.ChannelConfig)
			if t.channels == 0 {
				t.channels = 2
			}
			t.objectType = hdr.ObjectType
			t.frameDur90 = aacFrameSamples * tsClockRate / int64(t.sampleRate)
			if hasPTS {
				pesTS = t.last90 * int64(t.sampleRate) / tsClockRate
			}
		}
		// The PES timestamp belongs to the first frame that starts in this PES,
		// not to one carried over from the previous PES.
		dts := t.nextAudioDTS
		if !timestamped && skip >= carried {
			timestamped = true
			// Follow the PES timestamp only on the first frame or a real gap, so
			// rounding jitter doesn't produce uneven frame durations.
			if len(t.dts) == 0 || pesTS > dts+aacFrameSamples/2 {
				dts = pesTS
			}
		}
		if len(t.dts) == 0 {
			t.minPTS = dts
		}
		t.nextAudioDTS = dts + aacFrameSamples
		payload := data[skip+int(hdr.HeaderLength) : skip+frameLen]
		if err := rm.writeSample(t, payload, dts); err != nil {
			return err
		}
		data = data[skip+frameLen:]
		carried -= skip + frameLen
	}
	return nil
}

func cloneAll(nalus [][]byte) [][]byte {
	out := make([][]byte, len(nalus))
	for i, n := range nalus {
		out[i] = slices.Clone(n)
	}
	return out
}

// writeSample appends a sample to the mdat. Consecutive samples of the same
// track share a chunk, so the file interleaves the way the TS did.
func (rm *remuxer) writeSample(t *remuxTrack, data []byte, dts int64) error {
	if rm.lastTrack != t || len(t.chunks) == 0 {
		t.chunks = append(t.chunks, remuxChunk{offset: rm.pos})
		rm.lastTrack = t
	}
	t.chunks[len(t.chunks)-1].samples++
	t.sizes = append(t.sizes, uint32(len(data)))
	t.dts = append(t.dts, dts)
	return rm.write(data)
}

// duration returns the track's media duration in its timescale.
func (t *remuxTrack) duration() int64 {
	n := len(t.dts)
	return t.dts[n-1] + t.lastSampleDuration() - t.dts[0]
}

func (t *remuxTrack) lastSampleDuration() int64 {
	if !t.video {
		return aacFrameSamples
	}
	if n := len(t.dts); n > 1 {
		return t.dts[n-1] - t.dts[n-2]
	}
	return defaultFrameDur90
}

// startTime90 is when the track's first sample is presented, in the 90 kHz clock.
func (t *remuxTrack) startTime90() int64 {
	return t.minPTS * tsClockRate / int64(t.timescale)
}

func (rm *remuxer) finish(chapters []VideoChapter) error {
	var tracks []*remuxTrack
	for _, t := range []*remuxTrack{rm.video, rm.audio} {
		if t != nil && len(t.sizes) > 0 {
			tracks = append(tracks, t)
		}
	}
	if len(tracks) == 0 {
		return errors.New("no H.264 video or AAC audio found in stream")
	}
	if rm.video != nil && len(rm.video.sizes) > 0 && rm.video.sps == nil {
		return errors.New("no H.264 parameter sets found in video stream")
	}
	start90 := tracks[0].startTime90()
	for _, t := range tracks[1:] {
		start90 = min(start90, t.startTime90())
	}
	moov := mp4.NewMoovBox()
	mvhd := mp4.CreateMvhd()
	mvhd.Timescale = movieTimescale
	moov.AddChild(mvhd)
	var movieDur uint64
	traks := make([]*mp4.TrakBox, len(tracks))
	for i, t := range tracks {
		t.trackID = uint32(i + 1)
		trak, dur, err := t.buildTrak(start90)
		if err != nil {
			return err
		}
		traks[i] = trak
		movieDur = max(movieDur, dur)
	}

	marks := chapterMarks(chapters, movieDur)
	var chapTrak *mp4.TrakBox
	if len(marks) > 0 {
		chapID := uint32(len(tracks) + 1)
		var err error
		if chapTrak, err = rm.writeChapterTrack(chapID, marks, movieDur); err != nil {
			return err
		}
		for _, trak := range traks {
			addChapterRef(trak, chapID)
		}
	}
	for _, trak := range traks {
		moov.AddChild(trak)
	}
	if chapTrak != nil {
		moov.AddChild(chapTrak)
	}
	mvhd.Duration = movieDur
	mvhd.NextTrackID = uint32(len(moov.Traks) + 1)
	if len(marks) > 0 {
		udta := &mp4.UdtaBox{}
		udta.AddChild(rawBox("chpl", chplPayload(marks)))
		moov.AddChild(udta)
	}

	if err := rm.w.Flush(); err != nil {
		return err
	}
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, rm.pos-rm.mdatStart)
	if _, err := rm.f.WriteAt(size, int64(rm.mdatStart)+8); err != nil {
		return err
	}
	if err := moov.Encode(rm.w); err != nil {
		return err
	}
	return rm.w.Flush()
}

// toMovieTime converts a duration in timescale units to movie (millisecond) units.
func toMovieTime(d int64, timescale uint32) uint64 {
	return uint64(math.Round(float64(d) * movieTimescale / float64(timescale)))
}

// buildTrak returns the trak box for t and its duration in the movie timescale.
func (t *remuxTrack) buildTrak(start90 int64) (*mp4.TrakBox, uint64, error) {
	mediaDur := t.duration()
	mediaTime := t.minPTS - t.dts[0]
	emptyDur := uint64(math.Round(float64(t.startTime90()-start90) * movieTimescale / tsClockRate))
	presentDur := toMovieTime(mediaDur-mediaTime, t.timescale)

	trak := &mp4.TrakBox{}
	tkhd := mp4.CreateTkhd()
	tkhd.Flags = 0x3 // enabled, in movie
	tkhd.TrackID = t.trackID
	tkhd.Duration = emptyDur + presentDur
	trak.AddChild(tkhd)
	trak.AddChild(editList(emptyDur, presentDur, mediaTime))

	mdia := &mp4.MdiaBox{}
	mdhd := &mp4.MdhdBox{Timescale: t.timescale, Duration: uint64(mediaDur)}
	if mediaDur > math.MaxUint32 {
		mdhd.Version = 1
	}
	mdhd.SetLanguage("und")
	mdia.AddChild(mdhd)
	minf := mp4.NewMinfBox()
	var entry mp4.Box
	if t.video {
		hdlr, _ := mp4.CreateHdlr("vide")
		hdlr.Name = "VideoHandler"
		mdia.AddChild(hdlr)
		minf.AddChild(mp4.CreateVmhd())
		avcC, err := mp4.CreateAvcC(t.sps, t.pps, true)
		if err != nil {
			return nil, 0, err
		}
		sps, err := avc.ParseSPSNALUnit(t.sps[0], false)
		if err != nil {
			return nil, 0, err
		}
		width, height := sps.Width, sps.Height
		if vui := sps.VUI; vui != nil && vui.SampleAspectRatioWidth > 0 && vui.SampleAspectRatioHeight > 0 {
			width = width * vui.SampleAspectRatioWidth / vui.SampleAspectRatioHeight
		}
		vse := mp4.CreateVisualSampleEntryBox("avc1", uint16(sps.Width), uint16(sps.Height), avcC)
		vse.CompressorName = ""
		entry = vse
		tkhd.Width = mp4.Fixed32(width << 16)
		tkhd.Height = mp4.Fixed32(height << 16)
	} else {
		hdlr, _ := mp4.CreateHdlr("soun")
		hdlr.Name = "SoundHandler"
		mdia.AddChild(hdlr)
		minf.AddChild(mp4.CreateSmhd())
		tkhd.Volume = 0x0100
		asc := aac.AudioSpecificConfig{
			ObjectType:           t.objectType,
			ChannelConfiguration: byte(t.channels),
			SamplingFrequency:    t.sampleRate,
		}
		var ascBuf bytes.Buffer
		if err := asc.Encode(&ascBuf); err != nil {
			return nil, 0, err
		}
		entry = mp4.CreateAudioSampleEntryBox("mp4a", uint16(t.channels), 16,
			uint16(min(t.sampleRate, math.MaxUint16)), mp4.CreateEsdsBox(ascBuf.Bytes()))
	}
	dinf := &mp4.DinfBox{}
	dinf.AddChild(mp4.CreateDref())
	minf.AddChild(dinf)
	minf.AddChild(t.buildStbl(entry))
	mdia.AddChild(minf)
	trak.AddChild(mdia)
	return trak, tkhd.Duration, nil
}

// editList delays the track by emptyDur and skips the first mediaTime units of
// media, which video needs when B-frames make presentation start after decoding.
func editList(emptyDur, presentDur uint64, mediaTime int64) *mp4.EdtsBox {
	elst := &mp4.ElstBox{}
	if emptyDur > 0 {
		elst.Entries = append(elst.Entries, mp4.ElstEntry{SegmentDuration: emptyDur, MediaTime: -1, MediaRateInteger: 1})
	}
	elst.Entries = append(elst.Entries, mp4.ElstEntry{SegmentDuration: presentDur, MediaTime: mediaTime, MediaRateInteger: 1})
	if mediaTime > math.MaxInt32 || presentDur > math.MaxUint32 {
		elst.Version = 1
	}
	edts := &mp4.EdtsBox{}
	edts.AddChild(elst)
	return edts
}

func (t *remuxTrack) buildStbl(entry mp4.Box) *mp4.StblBox {
	stbl := mp4.NewStblBox()
	stsd := mp4.NewStsdBox()
	stsd.AddChild(entry)
	stbl.AddChild(stsd)

	durations := make([]uint32, len(t.dts))
	for i := range t.dts {
		if i+1 < len(t.dts) {
			durations[i] = uint32(t.dts[i+1] - t.dts[i])
		} else {
			durations[i] = uint32(t.lastSampleDuration())
		}
	}
	stbl.AddChild(sttsFor(durations))
	if t.video && len(t.syncs) < len(t.sizes) {
		stbl.AddChild(&mp4.StssBox{SampleNumber: t.syncs})
	}
	if t.video && slices.ContainsFunc(t.ctsOffsets, func(o int32) bool { return o != 0 }) {
		stbl.AddChild(cttsFor(t.ctsOffsets))
	}
	stbl.AddChild(stscFor(t.chunks))
	stbl.AddChild(stszFor(t.sizes))
	offsets := make([]uint64, len(t.chunks))
	for i, c := range t.chunks {
		offsets[i] = c.offset
	}
	stbl.AddChild(chunkOffsetBox(offsets))
	return stbl
}

func sttsFor(durations []uint32) *mp4.SttsBox {
	stts := &mp4.SttsBox{}
	for i, d := range durations {
		if i > 0 && stts.SampleTimeDelta[len(stts.SampleTimeDelta)-1] == d {
			stts.SampleCount[len(stts.SampleCount)-1]++
			continue
		}
		stts.SampleCount = append(stts.SampleCount, 1)
		stts.SampleTimeDelta = append(stts.SampleTimeDelta, d)
	}
	return stts
}

func cttsFor(offsets []int32) *mp4.CttsBox {
	ctts := &mp4.CttsBox{}
	var counts []uint32
	var values []int32
	for i, o := range offsets {
		if i > 0 && values[len(values)-1] == o {
			counts[len(counts)-1]++
			continue
		}
		counts = append(counts, 1)
		values = append(values, o)
	}
	if slices.ContainsFunc(values, func(o int32) bool { return o < 0 }) {
		ctts.Version = 1
	}
	_ = ctts.AddSampleCountsAndOffset(counts, values)
	return ctts
}

func stscFor(chunks []remuxChunk) *mp4.StscBox {
	stsc := &mp4.StscBox{}
	for i, c := range chunks {
		if i > 0 && chunks[i-1].samples == c.samples {
			continue
		}
		_ = stsc.AddEntry(uint32(i+1), c.samples, 1)
	}
	return stsc
}

func stszFor(sizes []uint32) *mp4.StszBox {
	stsz := &mp4.StszBox{SampleNumber: uint32(len(sizes))}
	if len(sizes) > 0 && !slices.ContainsFunc(sizes, func(s uint32) bool { return s != sizes[0] }) {
		stsz.SampleUniformSize = sizes[0]
		return stsz
	}
	stsz.SampleSize = sizes
	return stsz
}

// chunkOffsetBox uses 32-bit offsets when they fit, like ffmpeg, and 64-bit ones otherwise.
func chunkOffsetBox(offsets []uint64) mp4.Box {
	if len(offsets) == 0 || offsets[len(offsets)-1] <= math.MaxUint32 {
		stco := &mp4.StcoBox{ChunkOffset: make([]uint32, len(offsets))}
		for i, o := range offsets {
			stco.ChunkOffset[i] = uint32(o)
		}
		return stco
	}
	return &mp4.Co64Box{ChunkOffset: offsets}
}

func rawBox(boxType string, payload []byte) *mp4.UnknownBox {
	return mp4.CreateUnknownBox(boxType, uint64(8+len(payload)), payload)
}

type chapterMark struct {
	startMs uint64
	title   string
}

// chapterMarks orders the chapters, keeps the last of any that share a start
// time (as the ffmpeg path did), and drops any starting after the end.
func chapterMarks(chapters []VideoChapter, movieDur uint64) []chapterMark {
	sorted := slices.Clone(chapters)
	slices.SortStableFunc(sorted, func(a, b VideoChapter) int {
		return cmp.Compare(a.ChapterSeconds, b.ChapterSeconds)
	})
	var marks []chapterMark
	for _, c := range sorted {
		start := uint64(math.Round(max(c.ChapterSeconds, 0) * movieTimescale))
		if start >= movieDur {
			break
		}
		if n := len(marks); n > 0 && marks[n-1].startMs == start {
			marks = marks[:n-1]
		}
		marks = append(marks, chapterMark{startMs: start, title: c.ChapterName})
	}
	return marks
}

// chplPayload builds a Nero chapter box as ffmpeg writes it, with times in 100ns units.
func chplPayload(marks []chapterMark) []byte {
	var b bytes.Buffer
	b.Write([]byte{1, 0, 0, 0, 0, 0, 0, 0}) // version 1, flags, reserved
	n := min(len(marks), 255)
	b.WriteByte(byte(n))
	for _, m := range marks[:n] {
		binary.Write(&b, binary.BigEndian, m.startMs*10000)
		title, _ := truncate(m.title, 255)
		b.WriteByte(byte(len(title)))
		b.WriteString(title)
	}
	return b.Bytes()
}

// writeChapterTrack appends the chapter titles to the mdat and returns a
// QuickTime chapter text track for them, built like ffmpeg's.
func (rm *remuxer) writeChapterTrack(trackID uint32, marks []chapterMark, movieDur uint64) (*mp4.TrakBox, error) {
	encd := []byte{0, 0, 0, 0x0c, 'e', 'n', 'c', 'd', 0, 0, 1, 0}
	t := &remuxTrack{trackID: trackID, timescale: movieTimescale}
	t.chunks = []remuxChunk{{offset: rm.pos, samples: uint32(len(marks))}}
	rm.lastTrack = nil
	durations := make([]uint32, len(marks))
	for i, m := range marks {
		end := movieDur
		if i+1 < len(marks) {
			end = marks[i+1].startMs
		}
		durations[i] = uint32(end - m.startMs)
		sample := make([]byte, 2, 2+len(m.title)+len(encd))
		binary.BigEndian.PutUint16(sample, uint16(min(len(m.title), math.MaxUint16)))
		sample = append(sample, m.title[:min(len(m.title), math.MaxUint16)]...)
		sample = append(sample, encd...)
		t.sizes = append(t.sizes, uint32(len(sample)))
		if err := rm.write(sample); err != nil {
			return nil, err
		}
	}

	trak := &mp4.TrakBox{}
	tkhd := mp4.CreateTkhd()
	tkhd.Flags = 0x2 // in movie, but not enabled for playback
	tkhd.TrackID = trackID
	tkhd.Duration = movieDur
	trak.AddChild(tkhd)
	trak.AddChild(editList(marks[0].startMs, movieDur-marks[0].startMs, 0))
	mdia := &mp4.MdiaBox{}
	mdhd := &mp4.MdhdBox{Timescale: movieTimescale, Duration: movieDur - marks[0].startMs}
	mdhd.SetLanguage("und")
	mdia.AddChild(mdhd)
	hdlr, err := mp4.CreateHdlr("text")
	if err != nil {
		return nil, err
	}
	hdlr.Name = "SubtitleHandler"
	mdia.AddChild(hdlr)
	minf := mp4.NewMinfBox()
	minf.AddChild(rawBox("gmhd", gmhdPayload))
	dinf := &mp4.DinfBox{}
	dinf.AddChild(mp4.CreateDref())
	minf.AddChild(dinf)
	stbl := mp4.NewStblBox()
	stsd := mp4.NewStsdBox()
	stsd.AddChild(rawBox("text", textSampleEntryPayload))
	stbl.AddChild(stsd)
	stbl.AddChild(sttsFor(durations))
	stbl.AddChild(stscFor(t.chunks))
	stbl.AddChild(stszFor(t.sizes))
	stbl.AddChild(chunkOffsetBox([]uint64{t.chunks[0].offset}))
	minf.AddChild(stbl)
	mdia.AddChild(minf)
	trak.AddChild(mdia)
	return trak, nil
}

func addChapterRef(trak *mp4.TrakBox, chapID uint32) {
	tref := &mp4.TrefBox{}
	tref.AddChild(&mp4.TrefTypeBox{Name: "chap", TrackIDs: []uint32{chapID}})
	// tref goes after tkhd and edts, before mdia.
	idx := slices.IndexFunc(trak.Children, func(b mp4.Box) bool { return b.Type() == "mdia" })
	trak.Children = slices.Insert(trak.Children, idx, mp4.Box(tref))
}

// gmhdPayload is the generic media header ffmpeg writes for chapter tracks,
// including the undocumented text atom QuickTime needs to show chapters.
var gmhdPayload = []byte{
	// gmin
	0x00, 0x00, 0x00, 0x18, 'g', 'm', 'i', 'n',
	0x00, 0x00, 0x00, 0x00, // version and flags
	0x00, 0x40, // graphics mode
	0x80, 0x00, 0x80, 0x00, 0x80, 0x00, // opcolor
	0x00, 0x00, // balance
	0x00, 0x00, // reserved
	// text
	0x00, 0x00, 0x00, 0x2c, 't', 'e', 'x', 't',
	0x00, 0x01,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x01,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x40, 0x00,
	0x00, 0x00,
}

// textSampleEntryPayload is the body of ffmpeg's QuickTime text sample entry.
var textSampleEntryPayload = []byte{
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // reserved
	0x00, 0x01, // data reference index
	0x00, 0x00, 0x00, 0x01, // displayFlags
	0x00, 0x00, // horizontal and vertical justification
	0x00, 0x00, 0x00, 0x00, // background color
	0x00, 0x00, 0x00, 0x00, // default text box top, left
	0x00, 0x00, 0x00, 0x00, // default text box bottom, right
	0x00, 0x00, 0x00, 0x00, // start and end char
	0x00, 0x01, // font ID
	0x00, 0x00, // font style flags and size
	0x00, 0x00, 0x00, 0x00, // foreground color
	0x00, 0x00, 0x00, 0x0d, 'f', 't', 'a', 'b', // font table
	0x00, 0x01, // entry count
	0x00, 0x01, // font ID
	0x00, // font name length
}
