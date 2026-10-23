package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.json")

	// 不存在 → 零值 + exists=false
	cfg, exists, err := loadConfig(path)
	if err != nil || exists || cfg.Server != "" {
		t.Fatalf("期望零值配置: cfg=%+v exists=%v err=%v", cfg, exists, err)
	}

	in := Config{Server: "http://10.0.0.5:8080", Token: "secret", Device: "mbp", PollMS: 300}
	if err := saveConfig(path, in); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	out, exists, err := loadConfig(path)
	if err != nil || !exists {
		t.Fatalf("loadConfig: exists=%v err=%v", exists, err)
	}
	if out != in {
		t.Fatalf("往返不一致: %+v != %+v", out, in)
	}

	// token 敏感，文件权限应为 0600
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("配置文件权限应为 0600，得到 %o", fi.Mode().Perm())
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://10.0.0.5:8080", "http://10.0.0.5:8080"},
		{"10.0.0.5:8080", "http://10.0.0.5:8080"},
		{"http://10.0.0.5:8080/", "http://10.0.0.5:8080"},
		{"  https://paste.lan  ", "https://paste.lan"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeServerURL(c.in); got != c.want {
			t.Errorf("normalizeServerURL(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func writeConfigFile(t *testing.T, dir string, cfg Config) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

// resolve 的核心行为：flag 优先于配置文件；首传配置要回写；引导分支不落盘。
func TestResolveFlagOverConfig(t *testing.T) {
	dir := t.TempDir()

	t.Run("无配置无flag→引导", func(t *testing.T) {
		r, err := resolve([]string{"-config", filepath.Join(dir, "none.json")})
		if err != nil {
			t.Fatal(err)
		}
		if !r.guide || r.saveWanted {
			t.Fatalf("期望 guide=true save=false，得到 guide=%v save=%v", r.guide, r.saveWanted)
		}
		if _, err := os.Stat(filepath.Join(dir, "none.json")); !os.IsNotExist(err) {
			t.Fatalf("引导分支不应写配置文件")
		}
	})

	t.Run("只给server缺token→仍引导", func(t *testing.T) {
		r, err := resolve([]string{"-config", filepath.Join(dir, "none2.json"), "-server", "http://10.0.0.5:8080"})
		if err != nil {
			t.Fatal(err)
		}
		if !r.guide || r.saveWanted {
			t.Fatalf("缺 token 应引导且不保存: %+v", r)
		}
	})

	t.Run("首传server+token→完整且需回写", func(t *testing.T) {
		r, err := resolve([]string{"-config", filepath.Join(dir, "none3.json"),
			"-server", "10.0.0.5:8080", "-token", "tk"})
		if err != nil {
			t.Fatal(err)
		}
		if r.guide || !r.saveWanted {
			t.Fatalf("期望 guide=false save=true: %+v", r)
		}
		if r.cfg.Server != "http://10.0.0.5:8080" {
			t.Fatalf("缺 scheme 应补全 http://，得到 %q", r.cfg.Server)
		}
		if r.cfg.Token != "tk" {
			t.Fatalf("token 未合并: %+v", r.cfg)
		}
	})

	t.Run("已有配置无flag→直接使用不回写", func(t *testing.T) {
		path := writeConfigFile(t, dir, Config{Server: "http://s1:8080", Token: "t1", Device: "d1", PollMS: 250})
		r, err := resolve([]string{"-config", path})
		if err != nil {
			t.Fatal(err)
		}
		if r.guide || r.saveWanted {
			t.Fatalf("完整配置不应引导/回写: %+v", r)
		}
		if r.cfg.Device != "d1" || r.cfg.PollMS != 250 {
			t.Fatalf("配置文件值未加载: %+v", r.cfg)
		}
	})

	t.Run("flag覆盖配置文件并回写", func(t *testing.T) {
		path := writeConfigFile(t, dir, Config{Server: "http://s1:8080", Token: "t1", Device: "d1", PollMS: 250})
		r, err := resolve([]string{"-config", path, "-server", "http://s2:9000", "-poll", "1s"})
		if err != nil {
			t.Fatal(err)
		}
		if r.cfg.Server != "http://s2:9000" || r.cfg.PollMS != 1000 {
			t.Fatalf("flag 应覆盖配置文件: %+v", r.cfg)
		}
		if r.cfg.Token != "t1" || r.cfg.Device != "d1" {
			t.Fatalf("未指定的 flag 应保留配置文件值: %+v", r.cfg)
		}
		if !r.saveWanted {
			t.Fatalf("有显式 flag 应回写配置")
		}
	})

	t.Run("install与uninstall互斥", func(t *testing.T) {
		if _, err := resolve([]string{"-install", "-uninstall"}); err == nil || errors.Is(err, errSilent) {
			t.Fatalf("应报错，得到 %v", err)
		}
	})

	t.Run("uninstall不需要配置", func(t *testing.T) {
		r, err := resolve([]string{"-config", filepath.Join(dir, "none4.json"), "-uninstall"})
		if err != nil {
			t.Fatal(err)
		}
		if !r.uninstall || r.guide || r.saveWanted {
			t.Fatalf("uninstall 不应依赖配置: %+v", r)
		}
	})

	t.Run("version提前退出", func(t *testing.T) {
		if _, err := resolve([]string{"-version"}); !errors.Is(err, errSilent) {
			t.Fatalf("期望 errSilent，得到 %v", err)
		}
	})
}
