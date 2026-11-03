package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWebConfiguration(t *testing.T) {
	c, err := Load("")
	if err != nil || c.Web.Listen != "" || c.Web.TokenEnv != "CAPYBARA_WEB_TOKEN" {
		t.Fatalf("默认配置改变: %+v %v", c.Web, err)
	}
	for _, test := range []struct {
		listen string
		valid  bool
	}{{"127.0.0.1:8080", true}, {"[::1]:8080", true}, {"0.0.0.0:8080", false}, {"192.168.1.2:8080", false}, {":8080", false}, {"invalid", false}} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err = os.WriteFile(path, []byte("web:\n  listen: \""+test.listen+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err = Load(path)
		if (err == nil) != test.valid {
			t.Fatalf("%s: %v", test.listen, err)
		}
	}
}
