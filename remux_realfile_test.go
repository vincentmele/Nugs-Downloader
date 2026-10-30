package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/asticode/go-astits"
)

// TestRemuxRealFileRoundTrip checks the remuxer against a real H.264/AAC MP4,
// such as one made by the ffmpeg path. It rebuilds the start of the file as an
// MPEG-TS stream the way Nugs serves it, remuxes that, and compares every
// sample and timestamp with the original. It's skipped unless NUGS_SAMPLE_MP4
// is set; NUGS_SAMPLE_SECONDS limits the length (default 1200) and
// NUGS_SAMPLE_OUT keeps the output in that directory.
func TestRemuxRealFileRoundTrip(t *testing.T) {
	src := os.Getenv("NUGS_SAMPLE_MP4")
	if src == "" {
		t.Skip("set NUGS_SAMPLE_MP4 to run")
	}
	seconds := 1200.0
	if s := os.Getenv("NUGS_SAMPLE_SECONDS"); s != "" {
		var err error
		if seconds, err = strconv.ParseFloat(s, 64); err != nil {
			t.Fatal(err)
		}
	}
	outDir := os.Getenv("NUGS_SAMPLE_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}

	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	orig, err := mp4.DecodeFile(in, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		t.Fatal(err)
	}
	var vTrak, aTrak *mp4.TrakBox
	for _, trak := range orig.Moov.Traks {
		switch trak.Mdia.Hdlr.HandlerType {
		case "vide":
			vTrak = trak
		case "soun":
			aTrak = trak
		}
	}
	if vTrak == nil || aTrak == nil || vTrak.Mdia.Minf.Stbl.Stsd.AvcX == nil || aTrak.Mdia.Minf.Stbl.Stsd.Mp4a == nil {
		t.Fatal("need an H.264 video track and an AAC audio track")
	}
	vScale := float64(vTrak.Mdia.Mdhd.Timescale)
	aScale := float64(aTrak.Mdia.Mdhd.Timescale)
	video := readSampleTable(t, vTrak, int64(seconds*vScale))
	audio := readSampleTable(t, aTrak, int64(seconds*aScale))

	// Rebuild TS timestamps from the original presentation timeline, including
	// each track's edit list delay.
	const base = 10 * tsClockRate
	vEmpty, vMediaTime := editOffsets(vTrak)
	aEmpty, _ := editOffsets(aTrak)
	asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(aTrak.Mdia.Minf.Stbl.Stsd.Mp4a.Esds.DecConfigDescriptor.DecSpecificInfo.DecConfig))
	if err != nil {
		t.Fatal(err)
	}

	tsPath := filepath.Join(outDir, "roundtrip.ts")
	tsFile, err := os.Create(tsPath)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriterSize(tsFile, 4<<20)
	mux := astits.NewMuxer(context.Background(), bw)
	mux.AddElementaryStream(astits.PMTElementaryStream{ElementaryPID: testVideoPID, StreamType: astits.StreamTypeH264Video})
	mux.AddElementaryStream(astits.PMTElementaryStream{ElementaryPID: testAudioPID, StreamType: astits.StreamTypeAACAudio})
	mux.SetPCRPID(testVideoPID)
	write := func(pid uint16, sid uint8, pts, dts int64, data []byte, key bool) {
		oh := &astits.PESOptionalHeader{MarkerBits: 2, PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS, PTS: &astits.ClockReference{Base: pts}}
		if dts != pts {
			oh.PTSDTSIndicator = astits.PTSDTSIndicatorBothPresent
			oh.DTS = &astits.ClockReference{Base: dts}
		}
		d := &astits.MuxerData{PID: pid, PES: &astits.PESData{Header: &astits.PESHeader{StreamID: sid, OptionalHeader: oh}, Data: data}}
		if key {
			d.AdaptationField = &astits.PacketAdaptationField{RandomAccessIndicator: true}
		}
		if _, err := mux.WriteData(d); err != nil {
			t.Fatal(err)
		}
	}
	vTS := func(i int) (pts, dts int64) {
		dts = base + vEmpty*tsClockRate/movieTimescale + int64(float64(video.dts[i]-vMediaTime)*tsClockRate/vScale)
		return dts + int64(float64(video.cts[i])*tsClockRate/vScale), dts
	}
	aTS := func(i int) int64 {
		return base + aEmpty*tsClockRate/movieTimescale + int64(float64(audio.dts[i])*tsClockRate/aScale)
	}
	const framesPerPES = 5
	for v, a := 0, 0; v < len(video.sizes) || a < len(audio.sizes); {
		vPTS, vDTS := int64(0), int64(0)
		if v < len(video.sizes) {
			vPTS, vDTS = vTS(v)
		}
		if v < len(video.sizes) && (a >= len(audio.sizes) || vDTS <= aTS(a)) {
			write(testVideoPID, 0xe0, vPTS, vDTS, avc.ConvertSampleToByteStream(video.read(t, in, v)), video.sync[v])
			v++
			continue
		}
		pts := aTS(a)
		var pes []byte
		for i := 0; i < framesPerPES && a < len(audio.sizes); i++ {
			payload := audio.read(t, in, a)
			hdr, err := aac.NewADTSHeader(asc.SamplingFrequency, asc.ChannelConfiguration, asc.ObjectType, uint16(len(payload)))
			if err != nil {
				t.Fatal(err)
			}
			pes = append(append(pes, hdr.Encode()...), payload...)
			a++
		}
		write(testAudioPID, 0xc0, pts, pts, pes, false)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	tsInfo, _ := tsFile.Stat()
	tsFile.Close()

	outPath := filepath.Join(outDir, "roundtrip.mp4")
	chapters := []VideoChapter{{ChapterSeconds: 0, ChapterName: "Set 1"}, {ChapterSeconds: seconds / 2, ChapterName: "Second half"}}
	start := time.Now()
	if err := remuxFile(t.Context(), tsPath, outPath, chapters); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("remuxed %d MB of TS in %v (%.0f MB/s): %d video and %d audio samples",
		tsInfo.Size()>>20, elapsed.Round(time.Millisecond), float64(tsInfo.Size())/(1<<20)/elapsed.Seconds(),
		len(video.sizes), len(audio.sizes))

	out, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	got, err := mp4.DecodeFile(out, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		t.Fatal(err)
	}
	gv := readSampleTable(t, got.Moov.Traks[0], -1)
	ga := readSampleTable(t, got.Moov.Traks[1], -1)
	compareTables(t, "video", in, out, video, gv, true)
	compareTables(t, "audio", in, out, audio, ga, false)

	ge, gm := editOffsets(got.Moov.Traks[0])
	ae, _ := editOffsets(got.Moov.Traks[1])
	t.Logf("edit lists: original video delay %dms media time %d, audio delay %dms; remuxed video %dms/%d, audio %dms",
		vEmpty, vMediaTime, aEmpty, ge, gm, ae)
	if gm != vMediaTime {
		t.Errorf("video media time = %d, want %d", gm, vMediaTime)
	}
	if d := (ae - ge) - (aEmpty - vEmpty); d < -1 || d > 1 {
		t.Errorf("audio/video offset changed by %d ms", d)
	}
}

type sampleTable struct {
	offsets []uint64
	sizes   []uint32
	dts     []int64
	cts     []int64
	sync    []bool
}

func (s *sampleTable) read(t *testing.T, f *os.File, i int) []byte {
	t.Helper()
	b := make([]byte, s.sizes[i])
	if _, err := f.ReadAt(b, int64(s.offsets[i])); err != nil {
		t.Fatal(err)
	}
	return b
}

// readSampleTable lists the samples of a track that start before limit (in the
// track timescale), or all of them when limit is negative.
func readSampleTable(t *testing.T, trak *mp4.TrakBox, limit int64) *sampleTable {
	t.Helper()
	stbl := trak.Mdia.Minf.Stbl
	var chunkOffsets []uint64
	if stbl.Co64 != nil {
		chunkOffsets = stbl.Co64.ChunkOffset
	} else {
		for _, o := range stbl.Stco.ChunkOffset {
			chunkOffsets = append(chunkOffsets, uint64(o))
		}
	}
	durations := sttsDurations(stbl.Stts)
	syncs := map[uint32]bool{}
	if stbl.Stss != nil {
		for _, n := range stbl.Stss.SampleNumber {
			syncs[n] = true
		}
	}
	s := &sampleTable{}
	nr := uint32(1)
	var dts int64
	for i, e := range stbl.Stsc.Entries {
		last := uint32(len(chunkOffsets))
		if i+1 < len(stbl.Stsc.Entries) {
			last = stbl.Stsc.Entries[i+1].FirstChunk - 1
		}
		for chunk := e.FirstChunk; chunk <= last; chunk++ {
			pos := chunkOffsets[chunk-1]
			for range e.SamplesPerChunk {
				if limit >= 0 && dts >= limit {
					return s
				}
				size := stbl.Stsz.GetSampleSize(int(nr))
				var cts int64
				if stbl.Ctts != nil {
					cts = int64(stbl.Ctts.GetCompositionTimeOffset(nr))
				}
				s.offsets = append(s.offsets, pos)
				s.sizes = append(s.sizes, size)
				s.dts = append(s.dts, dts)
				s.cts = append(s.cts, cts)
				s.sync = append(s.sync, stbl.Stss == nil || syncs[nr])
				pos += uint64(size)
				dts += int64(durations[nr-1])
				nr++
			}
		}
	}
	return s
}

// editOffsets returns a track's initial delay in movie time and its media start time.
func editOffsets(trak *mp4.TrakBox) (empty, mediaTime int64) {
	if trak.Edts == nil || len(trak.Edts.Elst) == 0 {
		return 0, 0
	}
	for _, e := range trak.Edts.Elst[0].Entries {
		if e.MediaTime == -1 {
			empty += int64(e.SegmentDuration)
			continue
		}
		return empty, e.MediaTime
	}
	return empty, 0
}

func compareTables(t *testing.T, name string, origF, gotF *os.File, want, got *sampleTable, exactTiming bool) {
	t.Helper()
	if len(got.sizes) != len(want.sizes) {
		t.Errorf("%s: %d samples, want %d", name, len(got.sizes), len(want.sizes))
		return
	}
	var dataDiffs, syncDiffs, timingDiffs int
	var maxDrift int64
	for i := range want.sizes {
		if !bytes.Equal(want.read(t, origF, i), got.read(t, gotF, i)) {
			dataDiffs++
		}
		if want.sync[i] != got.sync[i] {
			syncDiffs++
		}
		wantDTS, gotDTS := want.dts[i]-want.dts[0], got.dts[i]-got.dts[0]
		if wantDTS != gotDTS || want.cts[i] != got.cts[i] {
			timingDiffs++
			maxDrift = max(maxDrift, abs64(wantDTS-gotDTS))
		}
	}
	t.Logf("%s: %d samples, %d data differences, %d sync differences, %d timing differences (max %d ticks)",
		name, len(want.sizes), dataDiffs, syncDiffs, timingDiffs, maxDrift)
	if dataDiffs > 0 || syncDiffs > 0 {
		t.Errorf("%s samples changed", name)
	}
	if exactTiming && timingDiffs > 0 {
		t.Errorf("%s timestamps changed", name)
	}
	// ffmpeg can give the first few audio frames uneven durations (such as 1,
	// 1023, 1024, 2048), while every AAC frame really lasts 1024 samples, so
	// allow up to one frame of difference.
	if !exactTiming && maxDrift > aacFrameSamples {
		t.Errorf("%s timestamps drifted by %d ticks", name, maxDrift)
	}
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
