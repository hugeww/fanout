package main

import (
	"testing"
)

func TestRenameExitSuffix(t *testing.T) {
	cases := []struct{ remark, label, want string }{
		{"KR-248", "JP-132", "JP-132"},
		{"线路A-KR-248", "JP-132", "线路A-JP-132"},
		{"inbound-47525-KR-248", "JP-132", "inbound-47525-JP-132"},
		{"无格式", "JP-132", "无格式"},
		{"", "JP-132", ""},
	}
	for _, c := range cases {
		got := renameExitSuffix(c.remark, c.label)
		if got != c.want {
			t.Errorf("renameExitSuffix(%q) = %q, want %q", c.remark, got, c.want)
		}
	}
}
