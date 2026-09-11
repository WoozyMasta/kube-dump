// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

// Execute passes Kubernetes client settings to resource operations.
func (c *ResourceCommand) Execute(_ []string) error {
	c.Save.clientOptions = c.ClientOptions

	return nil
}

// Execute passes Kubernetes client settings to PVC operations.
func (c *PvcCommand) Execute(_ []string) error {
	c.Save.clientOptions = c.ClientOptions
	c.Restore.clientOptions = c.ClientOptions
	c.Restore.Dir.clientOptions = c.ClientOptions
	c.Restore.S3.clientOptions = c.ClientOptions

	return nil
}

// Execute passes Kubernetes client settings to image operations.
func (c *ImageCommand) Execute(_ []string) error {
	c.Save.clientOptions = c.ClientOptions

	return nil
}

// Execute passes resource settings to the selected resource destination.
func (c *ResourcesCommand) Execute(_ []string) error {
	c.Stdout.selection = c.Selection
	c.Stdout.resourceOptions = c.ResourceOptions
	c.Stdout.clientOptions = c.clientOptions
	c.Dir.selection = c.Selection
	c.Dir.resourceOptions = c.ResourceOptions
	c.Dir.clientOptions = c.clientOptions
	c.Git.selection = c.Selection
	c.Git.resourceOptions = c.ResourceOptions
	c.Git.clientOptions = c.clientOptions
	c.S3.selection = c.Selection
	c.S3.resourceOptions = c.ResourceOptions
	c.S3.clientOptions = c.clientOptions
	c.Archive.selection = c.Selection
	c.Archive.resourceOptions = c.ResourceOptions
	c.Archive.clientOptions = c.clientOptions
	c.ArchiveS3.selection = c.Selection
	c.ArchiveS3.resourceOptions = c.ResourceOptions
	c.ArchiveS3.clientOptions = c.clientOptions

	return nil
}

// Execute passes shared Kubernetes client settings to the selected PVC destination.
func (c *PvcSaveCommand) Execute(_ []string) error {
	c.Dir.clientOptions = c.clientOptions
	c.S3.clientOptions = c.clientOptions

	return nil
}

// Execute passes shared Kubernetes client settings to the selected PVC restore source.
func (c *PvcRestoreCommand) Execute(_ []string) error {
	c.Dir.clientOptions = c.clientOptions
	c.S3.clientOptions = c.clientOptions

	return nil
}

// Execute passes image settings to the selected image destination.
func (c *ImagesCommand) Execute(_ []string) error {
	c.Dir.imageOptions = c.ImageOptions
	c.Dir.clientOptions = c.clientOptions
	c.S3.imageOptions = c.ImageOptions
	c.S3.clientOptions = c.clientOptions

	return nil
}

// Execute passes the profile catalog directory to every profile operation.
func (c *ProfileCommand) Execute(_ []string) error {
	directory := c.Directory
	c.List.profileDirectory = directory
	c.Show.profileDirectory = directory
	c.Validate.profileDirectory = directory
	c.Which.profileDirectory = directory
	c.Copy.profileDirectory = directory
	c.Install.profileDirectory = directory
	c.Edit.profileDirectory = directory
	c.Remove.profileDirectory = directory

	return nil
}
