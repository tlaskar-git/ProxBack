package s3target

import "testing"

func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"s3.eu-central-003.backblazeb2.com": "https://s3.eu-central-003.backblazeb2.com",
		"https://s3.us-west-004.backblazeb2.com/": "https://s3.us-west-004.backblazeb2.com",
		"http://127.0.0.1:19000":                  "http://127.0.0.1:19000",
		"  minio.lab.local:9000 ":                 "https://minio.lab.local:9000",
	}
	for in, want := range cases {
		if got := NormalizeEndpoint(in); got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegionFromEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://s3.eu-central-003.backblazeb2.com": "eu-central-003",
		"https://s3.us-west-004.backblazeb2.com":    "us-west-004",
		"https://s3.eu-west-1.amazonaws.com":        "eu-west-1",
		"https://bucket.s3.eu-west-1.amazonaws.com": "", // virtual-hosted form: not "s3.<region>"
		"http://127.0.0.1:19000":                    "",
		"https://minio.lab.local:9000":              "",
		"":                                          "",
	}
	for in, want := range cases {
		if got := regionFromEndpoint(in); got != want {
			t.Errorf("regionFromEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
