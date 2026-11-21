package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVirtioTransportInImageImportAndRecipeContracts(t *testing.T) {
	for kind, file := range map[string]string{"image": "image-manifest.json", "import": "import-spec.json", "recipe": "build-prebuilt.json"} {
		t.Run(kind, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("../../examples", file))
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err = json.Unmarshal(b, &value); err != nil {
				t.Fatal(err)
			}
			boot := value["boot"].(map[string]any)
			for _, transport := range []string{"mmio", "pci", "invalid"} {
				boot["virtio_transport"] = transport
				b, _ = json.Marshal(value)
				err = Validate(kind, b)
				if (err == nil) != (transport != "invalid") {
					t.Fatal(kind, transport, err)
				}
			}
		})
	}
}

func TestStrictJSON(t *testing.T) {
	for _, s := range []string{`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `{} {}`, "{\"a\":\"\xff\"}", strings.Repeat(" ", MaxConfig+1), `[1,]`, `null null`} {
		t.Run(s[:min(30, len(s))], func(t *testing.T) {
			var v any
			if Decode([]byte(s), &v) == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
	var value any
	if err := Decode([]byte(strings.Repeat("[", 256)+"0"+strings.Repeat("]", 256)), &value); err == nil {
		t.Fatal("accepted excessive nesting")
	}
}
func TestDesignExamples(t *testing.T) {
	cases := map[string]string{"environment.json": "environment", "environment-aarch64.json": "environment", "environment-nat.json": "environment", "image-manifest.json": "image", "import-spec.json": "import", "build-prebuilt.json": "recipe", "build-kernel-source.json": "recipe", "event.json": "event", "result-up.json": "result", "result-failed.json": "result"}
	for file, kind := range cases {
		t.Run(file, func(t *testing.T) {
			b, e := os.ReadFile(filepath.Join("../../examples", file))
			if e != nil {
				t.Fatal(e)
			}
			if e = Validate(kind, b); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestSchemaNegatives(t *testing.T) {
	b, e := os.ReadFile("../../examples/environment.json")
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{strings.Replace(string(b), `"Environment"`, `"Other"`, 1), strings.Replace(string(b), `"memory_mib": 1024`, `"memory_mib": 1.5`, 1), strings.Replace(string(b), `"memory_mib": 1024`, `"memory_mib": null`, 1), strings.Replace(string(b), `"nat": false`, `"nat": false, "deploy": "oops"`, 1), strings.Replace(string(b), `"nodes"`, `"missing"`, 1)} {
		if Validate("environment", []byte(s)) == nil {
			t.Fatal("accepted schema violation")
		}
	}
}
func TestEnvironmentSemantics(t *testing.T) {
	b, _ := os.ReadFile("../../examples/environment.json")
	m, e := Environment(b, "/test/base")
	if e != nil {
		t.Fatal(e)
	}
	n := m["nodes"].([]any)[0].(map[string]any)
	if n["image"] != "/test/base/images/ubuntu-x86_64.fyimg" {
		t.Fatal(n)
	}
	for _, s := range []string{strings.Replace(string(b), `"node-2"`, `"node-1"`, 1), strings.Replace(string(b), `"auto"`, `"10.0.0.1/24"`, 1), strings.Replace(string(b), `"auto"`, `"192.168.0.0/15"`, 1), strings.Replace(string(b), `"dns": []`, `"dns": ["127.0.0.1"]`, 1)} {
		if _, e := Environment([]byte(s), "/tmp"); e == nil {
			t.Fatal("accepted semantic violation")
		}
	}
}
func TestPaths(t *testing.T) {
	for _, s := range []string{"", "/etc/passwd", "../x", "payload/../x", "payload//x", "payload/./x", "payload\\x", "a\x00b"} {
		if Path(s) == nil {
			t.Fatal(s)
		}
	}
	if e := Path("payload/rootfs.raw"); e != nil {
		t.Fatal(e)
	}
}
