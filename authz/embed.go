package authz

import _ "embed"

//go:embed permission-manifest.json
var PermissionManifest []byte
