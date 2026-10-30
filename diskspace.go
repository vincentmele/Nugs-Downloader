package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/dustin/go-humanize"
)

// videoSizeEstimate guesses a video's size from its advertised bitrate.
func videoSizeEstimate(v *m3u8.Variant, seconds float64) uint64 {
	bw := v.AverageBandwidth
	if bw == 0 {
		bw = v.Bandwidth
	}
	return uint64(float64(bw) / 8 * seconds)
}

// spaceForDownload decides whether free bytes are enough to download the rest
// of a video estimated at estimate bytes, of which have are on disk already.
// Converting afterwards needs about as much again, which only gets a warning
// since the download is kept. The estimate can run high when based on peak
// bitrate, so only a clear shortfall blocks the download.
func spaceForDownload(free, estimate, have uint64) (warning string, err error) {
	var remaining uint64
	if estimate > have {
		remaining = estimate - have
	}
	if free < remaining*4/5 {
		return "", fmt.Errorf("not enough disk space to download this video: it needs about %s and %s is free",
			humanize.Bytes(remaining), humanize.Bytes(free))
	}
	if free < remaining+estimate {
		return fmt.Sprintf("Warning: this video needs about %s of disk space to download and convert, and %s is free.",
			humanize.Bytes(remaining+estimate), humanize.Bytes(free)), nil
	}
	return "", nil
}

func checkDownloadSpace(tsPath string, estimate uint64) error {
	free, err := freeSpace(filepath.Dir(tsPath))
	if err != nil {
		return nil // can't tell, so don't get in the way
	}
	var have uint64
	if fi, err := os.Stat(tsPath); err == nil {
		have = uint64(fi.Size())
	}
	warning, err := spaceForDownload(free, estimate, have)
	if warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	return err
}

// checkConvertSpace checks there's room for the MP4, which is about as big as the TS.
func checkConvertSpace(tsPath string) error {
	free, err := freeSpace(filepath.Dir(tsPath))
	if err != nil {
		return nil
	}
	fi, err := os.Stat(tsPath)
	if err != nil {
		return err
	}
	if need := uint64(fi.Size()); free < need {
		return fmt.Errorf("not enough disk space to convert this video: it needs about %s and %s is free. "+
			"The download is kept, so free up space and run again", humanize.Bytes(need), humanize.Bytes(free))
	}
	return nil
}
