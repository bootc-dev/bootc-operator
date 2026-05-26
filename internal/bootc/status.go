// SPDX-License-Identifier: Apache-2.0

package bootc

import (
	"encoding/json"
	"fmt"
	"time"
)

// Status represents the top-level bootc status --json output.
type Status struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Spec       StatusSpec `json:"spec"`
	Status     StatusBody `json:"status"`
}

// StatusSpec is the spec section of bootc status output.
type StatusSpec struct {
	Image     *ImageReference `json:"image"`
	BootOrder string          `json:"bootOrder"`
}

// StatusBody is the status section of bootc status output.
type StatusBody struct {
	Staged           *BootEntry       `json:"staged"`
	Booted           *BootEntry       `json:"booted"`
	Rollback         *BootEntry       `json:"rollback"`
	OtherDeployments []BootEntry      `json:"otherDeployments,omitempty"`
	RollbackQueued   bool             `json:"rollbackQueued"`
	Type             *string          `json:"type"`
	UsrOverlay       *FilesystemOverlay `json:"usrOverlay"`
}

// BootEntry represents a single boot entry (booted, staged, or rollback).
type BootEntry struct {
	Image             *ImageStatus       `json:"image"`
	CachedUpdate      *ImageStatus       `json:"cachedUpdate"`
	Incompatible      bool               `json:"incompatible"`
	Pinned            bool               `json:"pinned"`
	SoftRebootCapable bool               `json:"softRebootCapable"`
	DownloadOnly      bool               `json:"downloadOnly"`
	Store             *string            `json:"store"`
	Ostree            *OstreeInfo        `json:"ostree"`
	Composefs         *ComposefsInfo     `json:"composefs"`
}

// ImageStatus describes the image in a boot entry.
type ImageStatus struct {
	Image        ImageReference `json:"image"`
	Version      *string        `json:"version"`
	Timestamp    *time.Time     `json:"timestamp"`
	ImageDigest  string         `json:"imageDigest"`
	Architecture string         `json:"architecture"`
}

// ImageReference holds the transport and image pullspec.
type ImageReference struct {
	Image     string          `json:"image"`
	Transport string          `json:"transport"`
	Signature *ImageSignature `json:"signature,omitempty"`
}

// OstreeInfo holds OSTree-specific metadata.
type OstreeInfo struct {
	Stateroot    string `json:"stateroot"`
	Checksum     string `json:"checksum"`
	DeploySerial int    `json:"deploySerial"`
}

// ComposefsInfo holds composefs-specific metadata.
type ComposefsInfo struct {
	Verity               string  `json:"verity"`
	BootType             string  `json:"bootType"`
	Bootloader           string  `json:"bootloader"`
	BootDigest           *string `json:"bootDigest"`
	MissingVerityAllowed bool    `json:"missingVerityAllowed"`
}

// ImageSignature describes the signature verification policy.
type ImageSignature struct {
	OstreeRemote    *string `json:"ostreeRemote,omitempty"`
	ContainerPolicy *bool  `json:"containerPolicy,omitempty"`
	Insecure        *bool  `json:"insecure,omitempty"`
}

// FilesystemOverlay describes a /usr overlay state.
type FilesystemOverlay struct {
	AccessMode  string `json:"accessMode"`
	Persistence string `json:"persistence"`
}

// ParseStatus parses raw bootc status --json output.
func ParseStatus(data []byte) (*Status, error) {
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing bootc status JSON: %w", err)
	}
	return &s, nil
}
