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
		var err error
		cloudConfig, err = loadCloudConfigFromSource(strings.TrimSpace(d.CloudInit))
		if err != nil {
			return nil, err
		}
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
		client := &http.Client{Timeout: 30 * time.Second}

		resp, reqErr := client.Get(source) //nolint:noctx,gosec // User-provided cloud-init URL.
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
	defaultUser := map[string]interface{}{
		"name":        sshUser,
		"lock_passwd": true,
		"sudo":        "ALL=(ALL) NOPASSWD:ALL",
	}

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

	for idx, entry := range usersList {
		userMap, mapOk := entry.(map[string]interface{})
		if !mapOk {
			continue
		}

		userName, nameOk := userMap["name"].(string)
		if !nameOk || userName != sshUser {
			continue
		}

		if _, hasLockPasswd := userMap["lock_passwd"]; !hasLockPasswd {
			userMap["lock_passwd"] = true
		}

		if _, hasSudo := userMap["sudo"]; !hasSudo {
			userMap["sudo"] = "ALL=(ALL) NOPASSWD:ALL"
		}

		switch keys := userMap["ssh_authorized_keys"].(type) {
		case []interface{}:
			for _, key := range keys {
				existingKey, isString := key.(string)
				if isString && strings.TrimSpace(existingKey) == sshPublicKey {
					usersList[idx] = userMap
					userdata["users"] = usersList
					return
				}
			}

			userMap["ssh_authorized_keys"] = append(keys, sshPublicKey)
		case []string:
			keysAsInterfaces := make([]interface{}, 0, len(keys)+1)
			found := false

			for _, key := range keys {
				keysAsInterfaces = append(keysAsInterfaces, key)
				if strings.TrimSpace(key) == sshPublicKey {
					found = true
				}
			}

			if !found {
				keysAsInterfaces = append(keysAsInterfaces, sshPublicKey)
			}

			userMap["ssh_authorized_keys"] = keysAsInterfaces
		default:
			userMap["ssh_authorized_keys"] = []interface{}{sshPublicKey}
		}

		usersList[idx] = userMap
		userdata["users"] = usersList
		return
	}

	defaultUser["ssh_authorized_keys"] = []interface{}{sshPublicKey}
	userdata["users"] = append(usersList, defaultUser)
}
