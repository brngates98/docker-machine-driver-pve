package driver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"
)

func TestGenerateCloudinitUserdata_Default(t *testing.T) {
	testDriver := newCloudInitTestDriver(t)

	userdata, err := testDriver.generateCloudinitUserdata()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(userdata, "#cloud-config\n"))

	parsed := decodeCloudConfig(t, userdata)
	require.Equal(t, testDriver.MachineName, parsed["hostname"])
	require.Equal(t, false, parsed["preserve_hostname"])
	require.Equal(t, true, parsed["create_hostname_file"])

	user := requireUser(t, parsed, testDriver.SSHUser)
	require.Equal(t, true, user["lock_passwd"])
	require.Equal(t, "ALL=(ALL) NOPASSWD:ALL", user["sudo"])
	require.Contains(t, toStringSlice(user["ssh_authorized_keys"]), "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey")
}

func TestGenerateCloudinitUserdata_WithCloudConfig(t *testing.T) {
	testDriver := newCloudInitTestDriver(t)
	testDriver.CloudConfig = `
#cloud-config
package_update: true
users:
  - name: ` + testDriver.SSHUser + `
    ssh_authorized_keys:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExistingKey
`

	userdata, err := testDriver.generateCloudinitUserdata()
	require.NoError(t, err)

	parsed := decodeCloudConfig(t, userdata)
	require.Equal(t, true, parsed["package_update"])

	user := requireUser(t, parsed, testDriver.SSHUser)
	keys := toStringSlice(user["ssh_authorized_keys"])
	require.Contains(t, keys, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExistingKey")
	require.Contains(t, keys, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey")
}

func TestLoadCloudConfigFromSource_FileAndURL(t *testing.T) {
	tempDir := t.TempDir()
	filepath := filepath.Join(tempDir, "cloud-config.yaml")
	fileContent := "#cloud-config\nruncmd:\n  - echo from-file\n"
	require.NoError(t, os.WriteFile(filepath, []byte(fileContent), 0o600))

	fromFile, err := loadCloudConfigFromSource(filepath)
	require.NoError(t, err)
	require.Equal(t, fileContent, fromFile)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("#cloud-config\nruncmd:\n  - echo from-url\n"))
	}))
	defer server.Close()

	fromURL, err := loadCloudConfigFromSource(server.URL)
	require.NoError(t, err)
	require.Contains(t, fromURL, "from-url")
}

func newCloudInitTestDriver(t *testing.T) *Driver {
	t.Helper()

	testDriver := NewDriver("cloudinit-test", t.TempDir())
	testDriver.SSHUser = defaultSSHUser

	publicKeyPath := testDriver.GetSSHPublicKeyPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(publicKeyPath), 0o700))
	require.NoError(
		t,
		os.WriteFile(publicKeyPath, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey\n"), 0o600),
	)

	return testDriver
}

func decodeCloudConfig(t *testing.T, cloudConfig string) map[string]interface{} {
	t.Helper()

	trimmedCloudConfig := strings.TrimPrefix(cloudConfig, "#cloud-config\n")
	result := map[string]interface{}{}
	require.NoError(t, yaml.Unmarshal([]byte(trimmedCloudConfig), &result))

	return result
}

func requireUser(t *testing.T, userdata map[string]interface{}, userName string) map[string]interface{} {
	t.Helper()

	usersRaw, ok := userdata["users"]
	require.True(t, ok)

	usersList, ok := usersRaw.([]interface{})
	require.True(t, ok)

	for _, userRaw := range usersList {
		userMap, userMapOk := userRaw.(map[string]interface{})
		if !userMapOk {
			continue
		}

		name, nameOk := userMap["name"].(string)
		if nameOk && name == userName {
			return userMap
		}
	}

	require.FailNowf(t, "user not found", "failed to find user '%s' in cloud-config users list", userName)

	return nil
}

func toStringSlice(value interface{}) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []interface{}:
		result := make([]string, 0, len(values))

		for _, item := range values {
			converted, ok := item.(string)
			if ok {
				result = append(result, converted)
			}
		}

		return result
	default:
		return nil
	}
}
