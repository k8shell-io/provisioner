package helm

import "testing"

func TestNormalizeManifest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "normalizes newlines trims removes empty lines separators and source headers",
			in: " \r\n" +
				"---\r\n" +
				"# Source: k8shell-workspace/templates/a.yaml\r\n" +
				"kind: Pod  \r\n" +
				"  # Source: keep-this (indented)\r\n" +
				"\r\n" +
				"---\n" +
				"\n" +
				"metadata:\n" +
				"  name: x\t\n" +
				"\n",
			want: "" +
				"kind: Pod\n" +
				"  # Source: keep-this (indented)\n" +
				"metadata:\n" +
				"  name: x\n",
		},
		{
			name: "empty input becomes empty",
			in:   " \n\n---\n\n# Source: foo\n\n",
			want: "",
		},
		{
			name: "keeps non-empty lines and trims trailing whitespace",
			in:   "a \t\nb\t \n",
			want: "a\nb\n",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeManifest(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeManifest() err=%v wantErr=%v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("normalizeManifest() mismatch\n--- got ---\n%q\n--- want ---\n%q", got, tt.want)
			}
		})
	}
}

func TestManifestsEqual(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		m1      string
		m2      string
		wantEq  bool
		wantErr bool
	}{
		{
			name: "equal after normalization (crlf trailing spaces extra separators and Source headers ignored)",
			m1: " \r\n" +
				"---\r\n" +
				"# Source: chart/templates/a.yaml\r\n" +
				"kind: Pod  \r\n" +
				"metadata:\r\n" +
				"  name: x\t\r\n" +
				"\r\n",
			m2: "\n" +
				"kind: Pod\n" +
				"metadata:\n" +
				"  name: x\n",
			wantEq: true,
		},
		{
			name: "different content should not be equal",
			m1: "" +
				"apiVersion: v1\n" +
				"kind: ConfigMap\n" +
				"metadata:\n" +
				"  name: a\n" +
				"data:\n" +
				"  x: \"1\"\n",
			m2: "" +
				"apiVersion: v1\n" +
				"kind: ConfigMap\n" +
				"metadata:\n" +
				"  name: a\n" +
				"data:\n" +
				"  x: \"2\"\n",
			wantEq: false,
		},
		{
			name: "indented '# Source:' line is NOT ignored",
			m1: "" +
				"apiVersion: v1\n" +
				"kind: ConfigMap\n" +
				"metadata:\n" +
				"  name: a\n" +
				"data:\n" +
				"  config.yaml: |-\n" +
				"    # Source: keep-this\n" +
				"    x: 1\n",
			m2: "" +
				"apiVersion: v1\n" +
				"kind: ConfigMap\n" +
				"metadata:\n" +
				"  name: a\n" +
				"data:\n" +
				"  config.yaml: |-\n" +
				"    x: 1\n",
			wantEq: false,
		},
		{
			name:   "both empty are equal",
			m1:     " \n\n---\n\n",
			m2:     "\r\n",
			wantEq: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eq, err := manifestsEqual(tt.m1, tt.m2)
			if (err != nil) != tt.wantErr {
				t.Fatalf("manifestsEqual() err=%v wantErr=%v", err, tt.wantErr)
			}
			if eq != tt.wantEq {
				t.Fatalf("manifestsEqual()=%v want %v", eq, tt.wantEq)
			}
		})
	}
}
