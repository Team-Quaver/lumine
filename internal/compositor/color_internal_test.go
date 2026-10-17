package compositor

import (
	"testing"

	"github.com/Team-Quaver/lumine/internal/config"
	"github.com/Team-Quaver/lumine/internal/wl"
)

// TestParametricBuild covers the mapping from wp_color_manager enums to the
// compositor's internal imageDesc, including rejection of features Lumine does
// not advertise.
func TestParametricBuild(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*paramCreatorHandler)
		wantTF  string
		wantPr  string
		wantErr bool
	}{
		{
			name:   "defaults to srgb",
			mutate: func(h *paramCreatorHandler) {},
			wantTF: "srgb", wantPr: "srgb",
		},
		{
			name: "hdr bt2020 pq",
			mutate: func(h *paramCreatorHandler) {
				h.SetTfNamed(nil, wl.WpColorManagerV1TransferFunctionSt2084Pq)
				h.SetPrimariesNamed(nil, wl.WpColorManagerV1PrimariesBt2020)
				h.SetLuminances(nil, 0, 1000, 100)
			},
			wantTF: "pq", wantPr: "bt2020",
		},
		{
			name: "unknown tf rejected",
			mutate: func(h *paramCreatorHandler) {
				h.SetTfNamed(nil, 9999)
			},
			wantErr: true,
		},
		{
			name: "tf power rejected",
			mutate: func(h *paramCreatorHandler) {
				h.SetTfPower(nil, 22)
			},
			wantErr: true,
		},
		{
			name: "explicit primaries rejected",
			mutate: func(h *paramCreatorHandler) {
				h.SetPrimaries(nil, 1, 2, 3, 4, 5, 6, 7, 8)
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &paramCreatorHandler{}
			tc.mutate(h)
			d, err := h.build()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", d)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.tf != tc.wantTF || d.primaries != tc.wantPr {
				t.Fatalf("desc = %+v, want tf=%s primaries=%s", d, tc.wantTF, tc.wantPr)
			}
		})
	}
}

// TestDescIdentity checks that identical descriptions share an identity and
// different ones do not.
func TestDescIdentity(t *testing.T) {
	a := imageDesc{tf: "pq", primaries: "bt2020", maxLum: 1000, refLum: 100}
	b := imageDesc{tf: "pq", primaries: "bt2020", maxLum: 1000, refLum: 100}
	c := imageDesc{tf: "srgb", primaries: "srgb", maxLum: 100, refLum: 100}
	if a.identity() != b.identity() {
		t.Fatal("identical descriptions have different identities")
	}
	if a.identity() == c.identity() {
		t.Fatal("different descriptions share an identity")
	}
	if a.identity() == 0 {
		t.Fatal("identity must be non-zero")
	}
}

// TestPreferredDesc checks the config drives the advertised output space.
func TestPreferredDesc(t *testing.T) {
	cfg := config.Default()
	cfg.Color.TFName = "pq"
	cfg.Color.Primaries = "bt2020"
	cfg.Color.MaxLuma = 1000
	d := preferredDesc(cfg)
	if d.tf != "pq" || d.primaries != "bt2020" || d.maxLum != 1000 {
		t.Fatalf("preferred = %+v", d)
	}
	// A nil config still yields a sane sRGB description.
	d = preferredDesc(nil)
	if d.tf != "srgb" || d.primaries != "srgb" || d.maxLum < d.refLum {
		t.Fatalf("default preferred = %+v", d)
	}
}
