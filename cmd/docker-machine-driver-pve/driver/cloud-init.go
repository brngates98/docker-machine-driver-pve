package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/rancher/machine/libmachine/log"
	yaml "gopkg.in/yaml.v3"
)

const (
	cloudInitHTTPTimeout = 30 * time.Second
	cloudInitSudoRule    = "ALL=(ALL) NOPASSWD:ALL"
)

// Configures cloud-init for the current machine.
func (d *Driver) setupCloudinit(ctx context.Context) error {
	machine, err := d.getCurrentMachine(ctx)
	if err != nil {
		return err
	}

	cloudinitMetadata, err := d.generateCloudinitMetadata()
	if err != nil {
		return fmt.Errorf("failed to generate cloud-init metadata: %w", err)
	}

	cloudinitUserdata, err := d.generateCloudinitUserdata()
	if err != nil {
		return fmt.Errorf("failed to generate cloud-init userdata: %w", err)
	}

	if err := machine.CloudInit(ctx, d.ISODeviceName, cloudinitUserdata, cloudinitMetadata, "", ""); err != nil {
		return fmt.Errorf("failed to configure cloud-init for Proxmox VE virtual machine ID='%d': %w", machine.VMID, err)
	}

	return nil
}

// Blocks until cloud-init finishes setup on the current machine.
func (d *Driver) waitForCloudinit() error {
	ctx, cancel := context.WithTimeout(context.TODO(), pveTaskPollingTimeout)
	defer cancel()

	for {
		err := d.runCommandOnCurrentMachine("sudo cloud-init status --wait")
		if err == nil {
			return nil
		}

		if errors.Is(err, ErrNonZeroExitCode) {
			return fmt.Errorf("cloud-init finished with non-zero exit code: %w", err)
		}

		log.Warn("failed to execute 'sudo cloud-init status --wait' over SSH, will retry:", err.Error())

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for cloud-init to finish: %w", context.DeadlineExceeded)
		case <-time.After(pveTaskPollingInterval):
			continue
		}
	}
}

// Removes cloud-init configuration from the current machine.
func (d *Driver) cleanupCloudinit(ctx context.Context) error {
	machine, err := d.getCurrentMachine(ctx)
	if err != nil {
		return err
	}

	if err := machine.UnmountCloudInitISO(ctx, d.ISODeviceName); err != nil {
		return fmt.Errorf("failed to remove cloud-init ISO: %w", err)
	}

	err = d.runTaskOnCurrentMachine(ctx, func(ctx context.Context, vm *proxmox.VirtualMachine) (*proxmox.Task, error) {
		return vm.RemoveTag(ctx, proxmox.MakeTag(proxmox.TagCloudInit))
	})
	if err != nil {
		return fmt.Errorf("failed to remove cloud-init tag: %w", err)
	}

	return nil
}

// Generates cloud-init metadatadata for the current machine.
func (d *Driver) generateCloudinitMetadata() (string, error) {
	metadata := map[string]interface{}{
		"instance-id": d.MachineName,
		"hostname":    d.MachineName,
	}

	metadataYAML, err := yaml.Marshal(&metadata)
	if err != nil {
		return "", fmt.Errorf("failed to marshal cloud-init metadata: %w", err)
	}

	return string(metadataYAML), nil
}

// Generates cloud-init userdata for the current machine.
func (d *Driver) generateCloudinitUserdata() (string, error) {
	sshPublicKey, err := os.ReadFile(d.GetSSHPublicKeyPath())
	if err != nil {
		return "", fmt.Errorf("failed to read machine's SSH public key: %w", err)
	}

	userdata, err := d.getBaseCloudinitUserdata()
	if err != nil {
		return "", err
	}

	defaultCloudInitUserdata(userdata, d.MachineName)
	upsertCloudInitSSHUser(userdata, d.SSHUser, strings.TrimSpace(string(sshPublicKey)))

	userdataYAML, err := yaml.Marshal(&userdata)
	if err != nil {
		return "", fmt.Errorf("failed to marshal cloud-init userdata: %w", err)
	}

	return fmt.Sprintf("#cloud-config\n%s", userdataYAML), nil
}

func (d *Driver) getBaseCloudinitUserdata() (map[string]interface{}, error) {
	cloudConfig := strings.TrimSpace(d.CloudConfig)
	if cloudConfig == "" {
		loadedCloudConfig, err := loadCloudConfigFromSource(strings.TrimSpace(d.CloudInit))
		if err != nil {
			return nil, err
		}

		cloudConfig = loadedCloudConfig
	}

	cloudConfig = strings.TrimSpace(cloudConfig)
	if cloudConfig == "" {
		return map[string]interface{}{}, nil
	}

	cloudConfig = strings.TrimPrefix(cloudConfig, "#cloud-config")
	cloudConfig = strings.TrimSpace(cloudConfig)

	userdata := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(cloudConfig), &userdata); err != nil {
		return nil, fmt.Errorf("failed to parse cloud-init user-data: %w", err)
	}

	return userdata, nil
}

func loadCloudConfigFromSource(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil
	}

	parsedURL, err := url.ParseRequestURI(source)
	if err == nil && parsedURL.Scheme != "" && parsedURL.Host != "" {
		client := &http.Client{Timeout: cloudInitHTTPTimeout}

		resp, reqErr := client.Get(source) //nolint:noctx // User-provided cloud-init URL.
		if reqErr != nil {
			return "", fmt.Errorf("failed to fetch cloud-init URL '%s': %w", source, reqErr)
		}

		defer resp.Body.Close()

		if resp.StatusCode >= http.StatusBadRequest {
			return "", fmt.Errorf("failed to fetch cloud-init URL '%s': status code %d", source, resp.StatusCode)
		}

		content, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", fmt.Errorf("failed to read cloud-init URL response '%s': %w", source, readErr)
		}

		return string(content), nil
	}

	content, readErr := os.ReadFile(source)
	if readErr != nil {
		return "", fmt.Errorf("failed to read cloud-init source '%s': %w", source, readErr)
	}

	return string(content), nil
}

func defaultCloudInitUserdata(userdata map[string]interface{}, machineName string) {
	if _, ok := userdata["hostname"]; !ok {
		userdata["hostname"] = machineName
	}

	if _, ok := userdata["preserve_hostname"]; !ok {
		userdata["preserve_hostname"] = false
	}

	if _, ok := userdata["create_hostname_file"]; !ok {
		userdata["create_hostname_file"] = true
	}
}

func upsertCloudInitSSHUser(userdata map[string]interface{}, sshUser, sshPublicKey string) {
	defaultUser := defaultCloudInitUser(sshUser)

	usersValue, usersSet := userdata["users"]
	if !usersSet {
		defaultUser["ssh_authorized_keys"] = []interface{}{sshPublicKey}
		userdata["users"] = []interface{}{defaultUser}

		return
	}

	usersList, ok := usersValue.([]interface{})
	if !ok {
		defaultUser["ssh_authorized_keys"] = []interface{}{sshPublicKey}
		userdata["users"] = []interface{}{usersValue, defaultUser}

		return
	}

	userIndex, userMap, found := findCloudInitUser(usersList, sshUser)
	if !found {
		defaultUser["ssh_authorized_keys"] = []interface{}{sshPublicKey}
		userdata["users"] = append(usersList, defaultUser)

		return
	}

	if _, hasLockPasswd := userMap["lock_passwd"]; !hasLockPasswd {
		userMap["lock_passwd"] = true
	}

	if _, hasSudo := userMap["sudo"]; !hasSudo {
		userMap["sudo"] = cloudInitSudoRule
	}

	userMap["ssh_authorized_keys"] = upsertCloudInitAuthorizedKeys(userMap["ssh_authorized_keys"], sshPublicKey)

	usersList[userIndex] = userMap
	userdata["users"] = usersList
}

func defaultCloudInitUser(sshUser string) map[string]interface{} {
	return map[string]interface{}{
		"name":        sshUser,
		"lock_passwd": true,
		"sudo":        cloudInitSudoRule,
	}
}

func findCloudInitUser(usersList []interface{}, sshUser string) (int, map[string]interface{}, bool) {
	for idx, entry := range usersList {
		userMap, mapOK := entry.(map[string]interface{})
		if !mapOK {
			continue
		}

		userName, nameOK := userMap["name"].(string)
		if !nameOK || userName != sshUser {
			continue
		}

		return idx, userMap, true
	}

	return -1, nil, false
}

func upsertCloudInitAuthorizedKeys(rawKeys interface{}, sshPublicKey string) []interface{} {
	keysAsInterfaces := cloudInitAuthorizedKeysToInterfaces(rawKeys)
	if cloudInitContainsAuthorizedKey(keysAsInterfaces, sshPublicKey) {
		return keysAsInterfaces
	}

	return append(keysAsInterfaces, sshPublicKey)
}

func cloudInitAuthorizedKeysToInterfaces(rawKeys interface{}) []interface{} {
	switch keys := rawKeys.(type) {
	case []interface{}:
		return keys
	case []string:
		keysAsInterfaces := make([]interface{}, 0, len(keys))

		for _, key := range keys {
			keysAsInterfaces = append(keysAsInterfaces, key)
		}

		return keysAsInterfaces
	default:
		return []interface{}{}
	}
}

func cloudInitContainsAuthorizedKey(keys []interface{}, expectedKey string) bool {
	for _, key := range keys {
		existingKey, isString := key.(string)
		if !isString {
			continue
		}

		if strings.TrimSpace(existingKey) == expectedKey {
			return true
		}
	}

	return false
}
