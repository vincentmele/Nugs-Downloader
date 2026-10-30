package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// defaultFolderFormat puts each release in a folder per artist, named by date
// so shows sort chronologically, e.g. "Goose/2026-09-02 Dillon Amphitheater, Dillon, CO".
const defaultFolderFormat = "{artist}/{date} {title}"

// Nugs starts show names with the date, e.g. "09/02/26 Dillon Amphitheater, Dillon, CO".
var leadingDateRegex = regexp.MustCompile(`^\d{1,2}/\d{1,2}/\d{2,4}\s+`)

// isoDate converts the date formats Nugs uses to YYYY-MM-DD, or returns "".
func isoDate(s string) string {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006/01/02", "1/2/2006", "01/02/06"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// releaseDate returns when a show was played, or when an album was released.
func releaseDate(meta *AlbArtResp) string {
	for _, s := range []string{meta.PerformanceDateFormatted, meta.ReleaseDateFormatted, meta.PerformanceDate} {
		if d := isoDate(s); d != "" {
			return d
		}
	}
	return ""
}

// releaseTitle is a release's name without the date Nugs puts in front of shows.
func releaseTitle(containerInfo string) string {
	return strings.TrimSpace(leadingDateRegex.ReplaceAllString(strings.TrimSpace(containerInfo), ""))
}

// releaseName is a release's date and title, e.g. "2026-09-02 Dillon Amphitheater, Dillon, CO".
func releaseName(date, containerInfo string) string {
	return strings.TrimSpace(date + " " + releaseTitle(containerInfo))
}

// releasePath returns where a release goes under outPath, following format:
// "/" separates folders, and {artist}, {date}, {title} and {container} (the
// name as Nugs gives it) are filled in. Each folder or file name is limited
// to maxLen bytes.
func releasePath(outPath, format string, meta *AlbArtResp, maxLen int) string {
	if format == "" {
		format = defaultFolderFormat
	}
	values := strings.NewReplacer(
		"{artist}", sanitise(strings.TrimSpace(meta.ArtistName)),
		"{date}", releaseDate(meta),
		"{title}", sanitise(releaseTitle(meta.ContainerInfo)),
		"{container}", sanitise(strings.TrimSpace(meta.ContainerInfo)),
	)
	parts := []string{outPath}
	for _, component := range strings.Split(format, "/") {
		component = strings.Join(strings.Fields(values.Replace(component)), " ")
		component = strings.Trim(component, " -")
		if component == "" {
			continue
		}
		component, _ = truncate(component, maxLen)
		parts = append(parts, sanitise(strings.TrimSpace(component)))
	}
	if len(parts) == 1 {
		name, _ := truncate(sanitise(strings.TrimSpace(meta.ArtistName+" - "+meta.ContainerInfo)), maxLen)
		parts = append(parts, name)
	}
	return filepath.Join(parts...)
}

// legacyReleasePath is where versions before folderFormat saved a release.
func legacyReleasePath(outPath string, meta *AlbArtResp, maxLen int) string {
	name, _ := truncate(meta.ArtistName+" - "+strings.TrimRight(meta.ContainerInfo, " "), maxLen)
	return filepath.Join(outPath, sanitise(name))
}

// adoptLegacyPath moves something an older version saved at oldPath to newPath,
// so a change of naming doesn't make a release download again.
func adoptLegacyPath(oldPath, newPath string) error {
	if oldPath == newPath {
		return nil
	}
	if _, err := os.Stat(newPath); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(oldPath); err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
		return err
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}
	fmt.Printf("Moved %s to %s.\n", oldPath, newPath)
	return nil
}
