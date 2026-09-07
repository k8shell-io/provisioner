// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import "testing"

func TestAttachInitScriptFiles(t *testing.T) {
	values := map[string]interface{}{
		"initScripts": []interface{}{
			map[string]interface{}{"name": "workspace", "script": "true"},
			map[string]interface{}{"name": "db-setup", "script": "true", "always": true},
		},
	}

	attachInitScriptFiles(values)

	scripts := values["initScripts"].([]interface{})
	want := []string{"__init_01_workspace", "__init_02_db-setup"}
	for i, w := range want {
		got := scripts[i].(map[string]interface{})["__file"]
		if got != w {
			t.Errorf("initScripts[%d].__file = %v, want %q", i, got, w)
		}
	}
}

func TestAttachInitScriptFilesNoScripts(t *testing.T) {
	// Missing key and wrong element types must not panic.
	attachInitScriptFiles(map[string]interface{}{})
	attachInitScriptFiles(map[string]interface{}{"initScripts": []interface{}{"nope", 42}})
}
