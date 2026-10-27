package handler

import (
	"reflect"
	"testing"
)

func TestParsePhoneArgs(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantPhone  string
		wantCtcode int64
		wantTarget string
		wantErr    bool
	}{
		{
			name:       "simple phone",
			input:      "/phone 13800138000",
			wantPhone:  "13800138000",
			wantCtcode: 86,
			wantTarget: "",
			wantErr:    false,
		},
		{
			name:       "phone with main target",
			input:      "/phone 13800138000 main",
			wantPhone:  "13800138000",
			wantCtcode: 86,
			wantTarget: "main",
			wantErr:    false,
		},
		{
			name:       "phone with json filename",
			input:      "/phone 13800138000 fan1.json",
			wantPhone:  "13800138000",
			wantCtcode: 86,
			wantTarget: "fan1.json",
			wantErr:    false,
		},
		{
			name:       "phone with alias",
			input:      "/phone 13800138000 fan2",
			wantPhone:  "13800138000",
			wantCtcode: 86,
			wantTarget: "fan2",
			wantErr:    false,
		},
		{
			name:       "international prefix +852",
			input:      "/phone +85298765432",
			wantPhone:  "98765432",
			wantCtcode: 852,
			wantTarget: "",
			wantErr:    false,
		},
		{
			name:       "international prefix +86",
			input:      "/phone +8613800138000",
			wantPhone:  "13800138000",
			wantCtcode: 86,
			wantTarget: "",
			wantErr:    false,
		},
		{
			name:       "explicit --ctcode flag",
			input:      "/phone 13800138000 --ctcode 852 main",
			wantPhone:  "13800138000",
			wantCtcode: 852,
			wantTarget: "main",
			wantErr:    false,
		},
		{
			name:       "login phone alias prefix",
			input:      "/login phone 13800138000 fan1.json",
			wantPhone:  "13800138000",
			wantCtcode: 86,
			wantTarget: "fan1.json",
			wantErr:    false,
		},
		{
			name:    "empty input",
			input:   "/phone",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phone, ctcode, target, err := parsePhoneArgs(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePhoneArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if phone != tt.wantPhone {
					t.Errorf("phone = %v, want %v", phone, tt.wantPhone)
				}
				if ctcode != tt.wantCtcode {
					t.Errorf("ctcode = %v, want %v", ctcode, tt.wantCtcode)
				}
				if target != tt.wantTarget {
					t.Errorf("target = %v, want %v", target, tt.wantTarget)
				}
			}
		})
	}
}

func TestMaskPhone(t *testing.T) {
	if got := maskPhone("13800138000"); got != "138****8000" {
		t.Errorf("maskPhone(13800138000) = %v, want 138****8000", got)
	}
	if got := maskPhone("12345"); got != "12345" {
		t.Errorf("maskPhone(12345) = %v, want 12345", got)
	}
}

func TestSplitCommandArgs(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "simple args",
			input: "musician-sign",
			want:  []string{"musician-sign"},
		},
		{
			name:  "args with flags",
			input: "playids -f fan1.json --limit 10",
			want:  []string{"playids", "-f", "fan1.json", "--limit", "10"},
		},
		{
			name:  "args with quoted spaces",
			input: `task --config "config/my task.yaml" -v`,
			want:  []string{"task", "--config", "config/my task.yaml", "-v"},
		},
		{
			name:    "unclosed quote",
			input:   `task --config "unclosed`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitCommandArgs(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("splitCommandArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStripAnsi(t *testing.T) {
	colored := "\x1b[32m[SUCCESS]\x1b[0m \x1b[1;31mError message\x1b[0m"
	expected := "[SUCCESS] Error message"
	if got := stripAnsi(colored); got != expected {
		t.Errorf("stripAnsi() = %q, want %q", got, expected)
	}
}

func TestGetLastLines(t *testing.T) {
	input := "line1\nline2\nline3\nline4\nline5"
	if got := getLastLines(input, 2); got != "line4\nline5" {
		t.Errorf("getLastLines(2) = %q, want %q", got, "line4\nline5")
	}
	if got := getLastLines(input, 10); got != input {
		t.Errorf("getLastLines(10) = %q, want %q", got, input)
	}
}

func TestParseLoginResult(t *testing.T) {
	sampleOutput := `Some logs...
NCMM_LOGIN_RESULT {"uid":888888,"nickname":"网易听歌人","accountPath":"${HOME}/cookie.json","main":true}
`
	res, ok := parseLoginResult(sampleOutput)
	if !ok {
		t.Fatalf("expected parseLoginResult to succeed")
	}
	if res.UID != 888888 {
		t.Errorf("UID = %v, want 888888", res.UID)
	}
	if res.Nickname != "网易听歌人" {
		t.Errorf("Nickname = %v, want '网易听歌人'", res.Nickname)
	}
	if !res.Main {
		t.Errorf("Main = false, want true")
	}
}
