package sandbox

import "testing"

func TestIsNetworkCommand(t *testing.T) {
	cases := []struct {
		command string
		want    bool
	}{
		{"curl https://example.com", true},
		{"curl", true},
		{"wget -O out http://x", true},
		{"ssh host", true},
		{"git push origin main", true},
		{"git pull", true},
		{"git clone https://github.com/x/y", true},
		{"npm install", true},
		{"npm install --save-dev jest", true},
		{"pip install requests", true},
		{"go get github.com/foo/bar", true},
		{"docker pull nginx", true},
		{"brew install jq", true},
		{"npx tsc", true},
		// Non-network commands.
		{"go build ./...", false},
		{"go test ./...", false},
		{"git status", false},
		{"git diff", false},
		{"git commit -m x", false},
		{"rm -rf build", false},
		{"ls -la", false},
		{"", false},
		// Word-boundary: a network prefix must not match a longer command.
		{"git pushd", false},
		{"curlx", false},
	}
	for _, c := range cases {
		if got := IsNetworkCommand(c.command); got != c.want {
			t.Errorf("IsNetworkCommand(%q) = %v, want %v", c.command, got, c.want)
		}
	}
}