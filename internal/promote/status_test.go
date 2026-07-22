package promote

import "testing"

func TestStatusOf(t *testing.T) {
	tests := []struct {
		name           string
		staging, prod  []string
		wantState      State
		wantStagingTag string
		wantProdTag    string
		wantNewProdTag string
	}{
		{
			name:           "in sync, plain SHAs",
			staging:        []string{"reg/foo:abc1234"},
			prod:           []string{"reg/foo:abc1234"},
			wantState:      InSync,
			wantStagingTag: "abc1234",
			wantProdTag:    "abc1234",
		},
		{
			name:           "out of sync, plain SHAs",
			staging:        []string{"reg/foo:abc1234"},
			prod:           []string{"reg/foo:def5678"},
			wantState:      OutOfSync,
			wantStagingTag: "abc1234",
			wantProdTag:    "def5678",
			wantNewProdTag: "abc1234",
		},
		{
			name:           "out of sync, suffixed",
			staging:        []string{"reg/foo:abc1234-staging"},
			prod:           []string{"reg/foo:def5678-prod"},
			wantState:      OutOfSync,
			wantStagingTag: "abc1234-staging",
			wantProdTag:    "def5678-prod",
			wantNewProdTag: "abc1234-prod",
		},
		{
			name:           "prod unpinned on mutable tag, staging plain SHA",
			staging:        []string{"reg/foo:abc1234"},
			prod:           []string{"reg/foo:latest"},
			wantState:      OutOfSync,
			wantStagingTag: "abc1234",
			wantProdTag:    "latest",
			wantNewProdTag: "abc1234",
		},
		{
			name:           "prod unpinned on mutable tag, staging suffixed",
			staging:        []string{"reg/foo:abc1234-staging"},
			prod:           []string{"reg/foo:prod"},
			wantState:      OutOfSync,
			wantStagingTag: "abc1234-staging",
			wantProdTag:    "prod",
			wantNewProdTag: "abc1234-prod",
		},
		{
			name:           "staging unparseable stays unknown",
			staging:        []string{"reg/foo:latest"},
			prod:           []string{"reg/foo:abc1234"},
			wantState:      Unknown,
			wantStagingTag: "latest",
			wantProdTag:    "abc1234",
		},
		{
			name:      "mid-deploy on staging",
			staging:   []string{"reg/foo:abc", "reg/foo:def"},
			prod:      []string{"reg/foo:abc"},
			wantState: MidDeploy,
		},
		{
			name:      "no staging pods",
			staging:   nil,
			prod:      []string{"reg/foo:abc1234"},
			wantState: Unknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StatusOf(tt.staging, tt.prod)
			if got.State != tt.wantState {
				t.Errorf("state = %v, want %v", got.State, tt.wantState)
			}
			if got.StagingTag != tt.wantStagingTag {
				t.Errorf("StagingTag = %q, want %q", got.StagingTag, tt.wantStagingTag)
			}
			if got.ProdTag != tt.wantProdTag {
				t.Errorf("ProdTag = %q, want %q", got.ProdTag, tt.wantProdTag)
			}
			if got.NewProdTag != tt.wantNewProdTag {
				t.Errorf("NewProdTag = %q, want %q", got.NewProdTag, tt.wantNewProdTag)
			}
		})
	}
}
