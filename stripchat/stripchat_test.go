package stripchat

import "testing"

func TestSelectBrowserPlaylistURLPrefersRootPlaylist(t *testing.T) {
	urls := []string{
		"https://media-hls.doppiocdn.net/b-hls-06/266279385/266279385_240p.m3u8?playlistType=lowLatency&psch=v2&pkey=abc",
		"https://media-hls.doppiocdn.net/b-hls-06/266279385/266279385.m3u8?playlistType=lowLatency&psch=v2&pkey=abc&_HLS_msn=1304&_HLS_part=1",
		"https://media-hls.doppiocdn.net/b-hls-06/266279385/266279385_480p.m3u8?playlistType=lowLatency&psch=v2&pkey=abc",
	}

	got := selectBrowserPlaylistURL(urls)
	want := "https://media-hls.doppiocdn.net/b-hls-06/266279385/266279385.m3u8?playlistType=lowLatency&psch=v2&pkey=abc&_HLS_msn=1304&_HLS_part=1"
	if got != want {
		t.Fatalf("selectBrowserPlaylistURL() = %q, want %q", got, want)
	}
}

func TestSelectBrowserPlaylistURLIgnoresNonPlaylists(t *testing.T) {
	urls := []string{
		"https://stripchat.com/api/front/v3/config/initial-dynamic?requestPath=%2Ffoo",
		"https://media-hls.doppiocdn.net/b-hls-06/media.mp4",
		"https://static-proxy.strpst.com/previews/example-thumb.jpg",
	}

	if got := selectBrowserPlaylistURL(urls); got != "" {
		t.Fatalf("selectBrowserPlaylistURL() = %q, want empty", got)
	}
}

func TestSelectBrowserPlaylistURLPrefersMediaHLSOverEdgeMaster(t *testing.T) {
	urls := []string{
		"https://edge-hls.doppiocdn.net/hls/266279385/master/266279385_auto.m3u8?playlistType=lowLatency",
		"https://media-hls.doppiocdn.net/b-hls-06/266279385/266279385.m3u8?playlistType=lowLatency&psch=v2&pkey=realkey&_HLS_msn=1304&_HLS_part=1",
	}

	got := selectBrowserPlaylistURL(urls)
	want := "https://media-hls.doppiocdn.net/b-hls-06/266279385/266279385.m3u8?playlistType=lowLatency&psch=v2&pkey=realkey&_HLS_msn=1304&_HLS_part=1"
	if got != want {
		t.Fatalf("selectBrowserPlaylistURL() = %q, want %q", got, want)
	}
}

func TestSelectSummaryCardURLPrefersPreviewThumbBig(t *testing.T) {
	urls := []string{
		"https://static-proxy.strpst.com/avatars/9/1/8/example-thumb",
		"https://static-proxy.strpst.com/previews/7/2/8/example-thumb-big",
		"https://static-proxy.strpst.com/previews/7/2/8/example-full",
	}

	got := selectSummaryCardURL(urls)
	want := "https://static-proxy.strpst.com/previews/7/2/8/example-thumb-big"
	if got != want {
		t.Fatalf("selectSummaryCardURL() = %q, want %q", got, want)
	}
}

func TestSelectLiveThumbURLPrefersDoppioThumb(t *testing.T) {
	urls := []string{
		"https://static-proxy.strpst.com/previews/7/2/8/example-thumb-big",
		"https://img.doppiocdn.net/thumbs/1788052059/266279385",
	}

	got := selectLiveThumbURL(urls)
	want := "https://img.doppiocdn.net/thumbs/1788052059/266279385"
	if got != want {
		t.Fatalf("selectLiveThumbURL() = %q, want %q", got, want)
	}
}
