package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testTarget = "http://127.0.0.1:8081"

func writeSchedule(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schedule.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustParse(t *testing.T, args ...string) config {
	t.Helper()
	cfg, err := parseConfig(append([]string{"--target", testTarget}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfigDefaults(t *testing.T) {
	cfg := mustParse(t)
	want := defaults()
	want.target = testTarget
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("defaults divergem:\n%+v\n%+v", cfg, want)
	}
	if cfg.keepalive || cfg.pipeline {
		t.Fatal("keepalive e pipeline devem começar desligados")
	}
	if cfg.pRepeat != defaultPRepeat || cfg.out != defaultOutPath || cfg.tokenPath != defaultTokenPath || cfg.tokenMode != tokenNone {
		t.Fatalf("defaults inesperados: %+v", cfg)
	}
}

func TestConfigRequiresTarget(t *testing.T) {
	if _, err := parseConfig(nil); err == nil {
		t.Fatal("sem --target devia falhar")
	}
}

func TestPipelineImpliesKeepalive(t *testing.T) {
	if cfg := mustParse(t, "--pipeline"); !cfg.keepalive {
		t.Fatal("--pipeline devia ligar keepalive")
	}
	if cfg := mustParse(t, "--keepalive"); !cfg.keepalive || cfg.pipeline {
		t.Fatal("--keepalive não devia ligar pipeline")
	}
}

func TestScheduleAppliesAndFlagWins(t *testing.T) {
	cases := []struct {
		flag, flagVal string
		key, schedVal string
		get           func(config) any
		wantSched     any
		wantFlag      any
	}{
		{"--per-24", "9", "per_24", "6", func(c config) any { return c.per24 }, 6, 9},
		{"--p-repeat", "0.05", "p_repeat", "0.01", func(c config) any { return c.pRepeat }, 0.01, 0.05},
		{"--pool-size", "300", "pool_size", "100", func(c config) any { return c.poolSize }, 100, 300},
		{"--client-24", "7", "client_24_count", "3", func(c config) any { return c.client24 }, 3, 7},
		{"--post-ratio", "0.2", "post_ratio", "0.1", func(c config) any { return c.postRatio }, 0.1, 0.2},
		{"--heavy-ratio", "0.8", "heavy_ratio", "0.6", func(c config) any { return c.heavyRatio }, 0.6, 0.8},
		{"--heavy-ms", "50", "heavy_ms", "75", func(c config) any { return c.heavyMS }, 75, 50},
		{"--waveform", "ramp", "waveform", `"pulse"`, func(c config) any { return c.waveform }, wavePulse, waveRamp},
		{"--churn", "0.4", "churn", "0.2", func(c config) any { return c.churn }, 0.2, 0.4},
		{"--sticky", "3", "sticky_s", "2", func(c config) any { return c.sticky }, 2.0, 3.0},
		{"--token-mode", "farm", "token_mode", `"valid-reuse"`, func(c config) any { return c.tokenMode }, tokenValidReuse, tokenFarm},
		{"--token-budget", "5", "token_budget", "4", func(c config) any { return c.tokenBudget }, 4, 5},
		{"--token-path", "/t2", "token_path", `"/t1"`, func(c config) any { return c.tokenPath }, "/t1", "/t2"},
	}
	for _, c := range cases {
		sched := writeSchedule(t, `{"`+c.key+`":`+c.schedVal+`,"token_budget":4}`)
		if got := c.get(mustParse(t, "--schedule", sched)); got != c.wantSched {
			t.Errorf("%s: schedule deu %v, esperava %v", c.key, got, c.wantSched)
		}
		if got := c.get(mustParse(t, "--schedule", sched, c.flag, c.flagVal, "--token-budget", "5")); got != c.wantFlag {
			t.Errorf("%s: flag deu %v, esperava %v (flag vence o arquivo)", c.flag, got, c.wantFlag)
		}
	}
}

func TestScheduleUserAgentsAndHeaders(t *testing.T) {
	sched := writeSchedule(t, `{"user_agents":["ua-1","ua-2"],"headers":[{"name":"X-A","value":"1"}]}`)
	cfg := mustParse(t, "--schedule", sched)
	if !reflect.DeepEqual(cfg.uas, []string{"ua-1", "ua-2"}) {
		t.Fatalf("uas: %v", cfg.uas)
	}
	if !reflect.DeepEqual(cfg.headers, [][2]string{{"X-A", "1"}}) {
		t.Fatalf("headers: %v", cfg.headers)
	}
}

func TestScheduleRejectsUnknownKeyAndBadJSON(t *testing.T) {
	for _, body := range []string{`{"p_repeet":0.05}`, `{"per_24":`} {
		_, err := parseConfig([]string{"--target", testTarget, "--schedule", writeSchedule(t, body)})
		if err == nil {
			t.Fatalf("schedule %q devia ser recusado", body)
		}
	}
	if _, err := parseConfig([]string{"--target", testTarget, "--schedule", filepath.Join(t.TempDir(), "nao-existe.json")}); err == nil {
		t.Fatal("schedule ausente devia falhar")
	}
}

func TestConfigValidationRanges(t *testing.T) {
	invalid := [][]string{
		{"--scenario", "s9"},
		{"--rate", "0"},
		{"--duration", "-1"},
		{"--warmup", "-1"},
		{"--p-repeat", "0.001"},
		{"--p-repeat", "0.07"},
		{"--per-24", "0"},
		{"--pool-size", "0"},
		{"--client-24", "-1"},
		{"--post-ratio", "-0.1"},
		{"--post-ratio", "1.1"},
		{"--heavy-ratio", "-0.1"},
		{"--heavy-ratio", "1.1"},
		{"--heavy-ms", "-1"},
		{"--churn", "1.5"},
		{"--churn", "-0.5"},
		{"--sticky", "-1"},
		{"--token-budget", "-1"},
		{"--token-budget", "1001"},
		{"--start-at", "-1"},
		{"--waveform", "zigzag"},
		{"--partial-timeout", "10s", "--total-timeout", "5s"},
	}
	for _, args := range invalid {
		if _, err := parseConfig(append([]string{"--target", testTarget}, args...)); err == nil {
			t.Errorf("%v devia ser recusado", args)
		}
	}
	valid := [][]string{
		{"--warmup", "0", "--client-24", "0", "--post-ratio", "1", "--heavy-ratio", "0", "--heavy-ms", "0", "--sticky", "0"},
		{"--p-repeat", "0.002"},
		{"--p-repeat", "0.06"},
		{"--partial-timeout", "5s", "--total-timeout", "5s"},
		{"--start-at", "0"},
		{"--token-budget", "1000"},
	}
	for _, args := range valid {
		mustParse(t, args...)
	}
}

func TestStartAtUpperBound(t *testing.T) {
	tooFar := strconv.FormatInt(time.Now().Add(2*time.Hour).UnixMilli(), 10)
	_, err := parseConfig([]string{"--target", testTarget, "--start-at", tooFar})
	if err == nil || !strings.Contains(err.Error(), "milissegundos") {
		t.Fatalf("start-at a 2 h devia ser recusado com a unidade na mensagem, veio %v", err)
	}
	mustParse(t, "--start-at", strconv.FormatInt(time.Now().Add(30*time.Minute).UnixMilli(), 10))
}

func TestTokenConfigValidation(t *testing.T) {
	invalid := [][]string{
		{"--token-mode", "bogus"},
		{"--token-mode", "farm"},
		{"--token-mode", "farm", "--token-budget", "0"},
		{"--token-path", "token"},
		{"--token-path", "/@evil.example/token"},
		{"--token-path", "/a//b"},
		{"--token-path", "/t?x=1"},
		{"--token-path", "/t#f"},
		{"--token-path", "http://evil.example/t"},
		{"--token-mode", "valid-reuse", "--duration", "300"},
		{"--token-mode", "farm", "--token-budget", "2", "--warmup", "30", "--duration", "241"},
	}
	for _, args := range invalid {
		if _, err := parseConfig(append([]string{"--target", testTarget}, args...)); err == nil {
			t.Errorf("%v devia ser recusado", args)
		}
	}
	mustParse(t, "--token-mode", "valid-reuse")
	mustParse(t, "--token-mode", "valid-reuse", "--duration", "270")
	mustParse(t, "--token-mode", "none", "--duration", "300")
	mustParse(t, "--token-mode", "farm", "--token-budget", "3", "--token-path", "/v1/token")
	mustParse(t, "--start-at", "1763164800000")
}

func TestPoolSpecPerScenario(t *testing.T) {
	cfg := defaults()
	cfg.poolSize = 100
	cfg.per24 = 8

	cfg.scenario = scenarioS1
	if s := cfg.poolSpec(); s.base != baseV4 || s.perGroup != 8 || s.groups != 13 || s.exclude != nil {
		t.Fatalf("s1: %+v", s)
	}
	cfg.scenario = scenarioS2a
	if s := cfg.poolSpec(); s.base != baseV6 || s.perGroup != 100 || s.groups != 1 {
		t.Fatalf("s2a: %+v", s)
	}
	cfg.scenario = scenarioS3
	cfg.client24 = 5
	s := cfg.poolSpec()
	if s.base != baseV4 || s.groups != 13 || !reflect.DeepEqual(s.exclude, rangeSet(1, 5)) {
		t.Fatalf("s3: %+v", s)
	}
}

func TestTimeoutDefaultsAreOrdered(t *testing.T) {
	cfg := mustParse(t)
	if cfg.partial != defaultPartialTimeout || cfg.total != defaultTotalTimeout || cfg.partial > cfg.total {
		t.Fatalf("timeouts: %v %v", cfg.partial, cfg.total)
	}
	cfg = mustParse(t, "--partial-timeout", "50ms", "--total-timeout", "200ms")
	if cfg.partial != 50*time.Millisecond || cfg.total != 200*time.Millisecond {
		t.Fatalf("flags de timeout ignoradas: %v %v", cfg.partial, cfg.total)
	}
}

func TestParseConfigRejectsUnknownFlag(t *testing.T) {
	if _, err := parseConfig([]string{"--target", testTarget, "--nope"}); err == nil {
		t.Fatalf("flag desconhecida devia falhar: %v", err)
	}
}
