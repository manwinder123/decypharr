package bridge

import "testing"

func TestDeInfringe(t *testing.T) {
	cases := []struct{ in, want string }{
		{"The.Matrix.1999.1080p.BluRay.x264-GROUP", "The.Matrix.1999.1080p.BluRay-x264-GROUP"},
		{"Show.S01E01.WEB-DL.1080p", "Show.S01E01.WEB.DL.1080p"},
		{"Movie.2024.BDRip.x264", "Movie.2024.BD-Rip.x264"},
		{"Film.WEBRip.1080p", "Film.WEB-Rip.1080p"},
		{"Already.Clean.HEVC.2160p", "Already.Clean.HEVC.2160p"},
		{"Bluray.x264 stays", "Bluray-x264 stays"},
		{"WEB-DL and BDRIP combo", "WEB.DL and BD-RIP combo"},
	}
	for _, c := range cases {
		got := DeInfringe(c.in)
		if got != c.want {
			t.Errorf("DeInfringe(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestBuildWebseedTorrent(t *testing.T) {
	files := []TorrentFile{
		{Path: "Movie 2024/Movie.WEB-DL.mkv", Size: 1024 * 1024 * 100, WebseedURL: "https://cdn.example.com/file1"},
		{Path: "Movie 2024/Subs.srt", Size: 1024, WebseedURL: "https://cdn.example.com/file2"},
	}
	data, hash, err := BuildWebseedTorrent(files, "Movie 2024")
	if err != nil {
		t.Fatalf("BuildWebseedTorrent: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty torrent")
	}
	if len(hash) != 40 {
		t.Errorf("hash len %d want 40", len(hash))
	}
	// De-infringe should have rewritten WEB-DL -> WEB.DL in the torrent
	if string(data) == "" || len(data) < 10 {
		t.Error("torrent too small")
	}
}
