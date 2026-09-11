// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/woozymasta/kube-dump/v2/internal/pvc"
)

// Execute backs up selected PVC data into per-claim timestamped directories.
func (c *PvcSaveDirCommand) Execute(_ []string) (err error) {
	operationID := pvc.NewOperationID()
	err = backupPVCData(
		c.context(),
		c.Path,
		operationID,
		c.PvcSaveOptions,
		c.clientOptions,
		c.progress,
		nil,
	)

	if rotateErr := func() error {
		if _, rotateErr := pvc.RotateArtifacts(c.Path, c.Keep); rotateErr != nil {
			return fmt.Errorf("rotate PVC artifacts: %w", rotateErr)
		}

		return nil
	}(); rotateErr != nil {
		if err != nil {
			return errors.Join(err, rotateErr)
		}

		return rotateErr
	}

	return err
}

// Execute backs up PVC data by running one S3 mover Pod per PVC.
// Each mover streams its mounted filesystem directly to its own timestamped object prefix;
// the client only coordinates Kubernetes resources and retention.
func (c *PvcSaveS3Command) Execute(_ []string) (err error) {
	baseStore, err := newS3Store(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("create PVC S3 store: %w", err)
	}

	operationID := pvc.NewOperationID()
	secretKey, err := readS3SecretKey(c.S3Destination)
	if err != nil {
		return fmt.Errorf("read PVC S3 credentials: %w", err)
	}

	moverEndpoint := c.Endpoint
	if c.PodEndpoint != "" {
		moverEndpoint = c.PodEndpoint
	}

	backupErr := backupPVCData(
		c.context(),
		"",
		operationID,
		c.PvcSaveOptions,
		c.clientOptions,
		c.progress,
		&pvc.S3MoverOptions{
			URI:       c.URI,
			Endpoint:  moverEndpoint,
			Insecure:  c.Insecure,
			Region:    c.Region,
			AccessKey: c.AccessKey,
			SecretKey: secretKey,
		},
	)

	rotateErr := func() error {
		if _, rotateErr := rotatePVCS3Runs(c.context(), baseStore, c.Keep); rotateErr != nil {
			return fmt.Errorf("rotate PVC S3 artifacts: %w", rotateErr)
		}

		return nil
	}()
	if backupErr != nil && rotateErr != nil {
		return errors.Join(backupErr, rotateErr)
	}
	if backupErr != nil {
		return backupErr
	}
	if rotateErr != nil {
		return rotateErr
	}

	return nil
}

// readS3SecretKey loads the optional static secret without exposing it to logs.
func readS3SecretKey(destination S3Destination) (string, error) {
	if destination.SecretKeyFile == "" {
		return "", nil
	}

	data, err := os.ReadFile(destination.SecretKeyFile)
	if err != nil {
		return "", err
	}

	secretKey := strings.TrimSpace(string(data))
	if secretKey == "" {
		return "", errors.New("S3 secret key file is empty")
	}

	return secretKey, nil
}
