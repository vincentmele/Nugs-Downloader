package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// Where Nugs keeps release artwork.
const imageBase = "https://secure.livedownloads.com"

// mediaTags is the metadata the built-in converter writes into HLS-only tracks
// and videos.
type mediaTags struct {
	Title  string
	Artist string
	Album  string
	Date   string // YYYY-MM-DD
	Track  int
	Tracks int
	art    *coverArt
}

// releaseTags returns the tags shared by everything in a release.
func releaseTags(meta *AlbArtResp) *mediaTags {
	date := releaseDate(meta)
	name := releaseName(date, meta.ContainerInfo)
	t := &mediaTags{Title: name, Artist: meta.ArtistName, Album: name, Date: date}
	switch {
	case meta.ExtImage != "":
		t.art = &coverArt{url: imageBase + "/images/shows/" + meta.ExtImage}
	case meta.Img.URL != "":
		t.art = &coverArt{url: imageBase + meta.Img.URL}
	}
	return t
}

// forTrack returns a copy of release tags for one of its tracks.
func (t *mediaTags) forTrack(title string, num, total int) *mediaTags {
	if t == nil {
		return nil
	}
	c := *t
	c.Title, c.Track, c.Tracks = title, num, total
	return &c
}

// coverArt downloads a release's artwork once, when first needed.
type coverArt struct {
	url  string
	once sync.Once
	data []byte
}

func (c *coverArt) get(ctx context.Context) []byte {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		data, err := fetchImage(ctx, c.url)
		if err != nil {
			fmt.Println("Couldn't get the cover art:", err)
			return
		}
		c.data = data
	})
	return c.data
}

func fetchImage(ctx context.Context, imageUrl string) ([]byte, error) {
	resp, err := httpGet(ctx, imageUrl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	if imageDataType(data) == 0 {
		return nil, errors.New("not a JPEG or PNG image")
	}
	return data, nil
}

// iTunes metadata data types.
const (
	dataImplicit = 0
	dataUTF8     = 1
	dataJPEG     = 13
	dataPNG      = 14
)

func imageDataType(img []byte) uint32 {
	switch {
	case bytes.HasPrefix(img, []byte{0xff, 0xd8, 0xff}):
		return dataJPEG
	case bytes.HasPrefix(img, []byte("\x89PNG\r\n\x1a\n")):
		return dataPNG
	}
	return 0
}

// mp4Box returns an MP4 box of the given four-byte type around payload.
func mp4Box(boxType string, payload ...[]byte) []byte {
	size := 8
	for _, p := range payload {
		size += len(p)
	}
	b := binary.BigEndian.AppendUint32(make([]byte, 0, size), uint32(size))
	b = append(b, boxType...)
	for _, p := range payload {
		b = append(b, p...)
	}
	return b
}

func ilstItem(itemType string, dataType uint32, value []byte) []byte {
	header := binary.BigEndian.AppendUint32(nil, dataType)
	header = append(header, 0, 0, 0, 0) // locale
	return mp4Box(itemType, mp4Box("data", header, value))
}

// metaPayload builds the body of an iTunes-style metadata box, laid out as
// ffmpeg writes it, for moov/udta/meta.
func metaPayload(t *mediaTags, cover []byte) []byte {
	var items [][]byte
	text := func(itemType, value string) {
		if value != "" {
			items = append(items, ilstItem(itemType, dataUTF8, []byte(value)))
		}
	}
	text("\xa9nam", t.Title)
	text("\xa9ART", t.Artist)
	text("aART", t.Artist)
	text("\xa9alb", t.Album)
	text("\xa9day", t.Date)
	if t.Track > 0 {
		trkn := []byte{0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint16(trkn[2:], uint16(t.Track))
		binary.BigEndian.PutUint16(trkn[4:], uint16(t.Tracks))
		items = append(items, ilstItem("trkn", dataImplicit, trkn))
	}
	if kind := imageDataType(cover); kind != 0 {
		items = append(items, ilstItem("covr", kind, cover))
	}
	text("\xa9too", "nugs_dl "+version)
	hdlr := mp4Box("hdlr", make([]byte, 8), []byte("mdirappl"), make([]byte, 9))
	return append(append([]byte{0, 0, 0, 0}, hdlr...), mp4Box("ilst", items...)...)
}

// coverFor returns the cover art for tags, fetching it if needed.
func (t *mediaTags) coverFor(ctx context.Context) []byte {
	if t == nil {
		return nil
	}
	return t.art.get(ctx)
}
