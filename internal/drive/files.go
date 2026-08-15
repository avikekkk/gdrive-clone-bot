package drive

import (
	"fmt"
	"strings"

	drivev3 "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"github.com/avisek/gdrive-clone-bot/internal/progress"
)

// ProgressFunc receives clone progress updates; force bypasses edit throttling.
type ProgressFunc func(p *progress.Clone, force bool)

// Precheck is the result of validating a clone request before starting it.
type Precheck struct {
	Name           string
	SourceID       string
	SourceMIMEType string
	AuthMode       string
}

// Result describes a finished clone.
type Result struct {
	ID       string
	Name     string
	MIMEType string
	URL      string
}

// CountResult summarizes the size and item counts of a Drive item.
type CountResult struct {
	ID       string
	Name     string
	MIMEType string
	Size     int64
	Files    int64
	Folders  int64
}

// DeleteTarget is a validated delete request, including how it will be removed.
type DeleteTarget struct {
	ID       string
	Name     string
	MIMEType string
	// Mode is "delete" for a hard delete or "trash" when that is all we may do.
	Mode     string
	AuthMode string
}

// DeleteResult describes a completed delete.
type DeleteResult struct {
	ID     string
	Name   string
	Type   string
	Action string
}

// itemSize returns the first positive size Drive reports for an item.
func itemSize(f *drivev3.File) int64 {
	if f == nil {
		return 0
	}
	if f.Size > 0 {
		return f.Size
	}
	if f.QuotaBytesUsed > 0 {
		return f.QuotaBytesUsed
	}
	return 0
}

func driveURLFor(f *drivev3.File) string {
	if f.WebViewLink != "" {
		return f.WebViewLink
	}
	if f.MimeType == FolderMIME {
		return "https://drive.google.com/drive/folders/" + f.Id
	}
	return "https://drive.google.com/file/d/" + f.Id + "/view"
}

// escapeQueryValue escapes a literal for use inside a Drive query string.
func escapeQueryValue(value string) string {
	escaped := value
	for _, char := range []string{`\`, `'`} {
		escaped = strings.ReplaceAll(escaped, char, `\`+char)
	}
	return strings.TrimSpace(escaped)
}

func (c *Cloner) filesGet(fileID, fields, resourceKey string) (*drivev3.File, error) {
	return retryDo(c.ctx, func() (*drivev3.File, error) {
		call := c.svc.Files.Get(fileID).
			SupportsAllDrives(true).
			Fields(googleapi.Field(fields)).
			Context(c.ctx)
		applyResourceKey(call.Header(), fileID, resourceKey)
		return call.Do()
	})
}

// filesGetWithFallback retries without the resource key, since some shared
// links carry keys that are unusable for the active principal.
func (c *Cloner) filesGetWithFallback(fileID, fields, resourceKey string) (*drivev3.File, error) {
	file, err := c.filesGet(fileID, fields, resourceKey)
	if err != nil && httpStatus(err) == 404 && resourceKey != "" {
		return c.filesGet(fileID, fields, "")
	}
	return file, err
}

// GetFileMetadata resolves a link and returns the item's metadata.
func (c *Cloner) GetFileMetadata(sourceLink string) (*drivev3.File, error) {
	return withAuthFallback(c, func() (*drivev3.File, error) {
		return c.getFileMetadata(sourceLink)
	})
}

func (c *Cloner) getFileMetadata(sourceLink string) (*drivev3.File, error) {
	parsed, err := ParseLink(sourceLink)
	if err != nil {
		return nil, err
	}
	file, err := c.filesGetWithFallback(parsed.FileID, metaFields, parsed.ResourceKey)
	if err != nil {
		return nil, normalizeHTTPError(err)
	}
	return file, nil
}

// ListFolderChildren lists direct children of a folder.
func (c *Cloner) ListFolderChildren(folderID, itemType string) ([]*drivev3.File, error) {
	switch itemType {
	case "all", "files", "folders":
	default:
		return nil, newError("Invalid folder listing type")
	}
	return withAuthFallback(c, func() ([]*drivev3.File, error) {
		return c.listChildren(folderID, itemType)
	})
}

// SetAnyoneReaderPermission grants public read access to an item.
func (c *Cloner) SetAnyoneReaderPermission(fileID string) (*drivev3.Permission, error) {
	return withAuthFallback(c, func() (*drivev3.Permission, error) {
		perm, err := retryDo(c.ctx, func() (*drivev3.Permission, error) {
			return c.svc.Permissions.
				Create(fileID, &drivev3.Permission{Role: "reader", Type: "anyone"}).
				SupportsAllDrives(true).
				Fields("id").
				Context(c.ctx).
				Do()
		})
		if err != nil {
			return nil, normalizeHTTPError(err)
		}
		return perm, nil
	})
}

// Account describes the Drive identity currently in use and its storage.
type Account struct {
	AuthMode    string
	DisplayName string
	Email       string

	// Limit is 0 when the identity has unlimited or pooled storage.
	Limit int64
	Usage int64
}

// About reports the active identity and its storage quota.
func (c *Cloner) About() (*Account, error) {
	return withAuthFallback(c, func() (*Account, error) {
		about, err := retryDo(c.ctx, func() (*drivev3.About, error) {
			return c.svc.About.Get().
				Fields("user(displayName,emailAddress),storageQuota(limit,usage)").
				Context(c.ctx).
				Do()
		})
		if err != nil {
			return nil, normalizeHTTPError(err)
		}

		account := &Account{AuthMode: c.AuthMode}
		if about.User != nil {
			account.DisplayName = about.User.DisplayName
			account.Email = about.User.EmailAddress
		}
		if about.StorageQuota != nil {
			account.Limit = about.StorageQuota.Limit
			account.Usage = about.StorageQuota.Usage
		}
		return account, nil
	})
}

// DestinationName returns the name of the configured destination folder.
func (c *Cloner) DestinationName(destinationID string) (string, error) {
	return withAuthFallback(c, func() (string, error) {
		meta, err := c.filesGet(destinationID, "id,name", "")
		if err != nil {
			return "", normalizeHTTPError(err)
		}
		return nameOrUnnamed(meta), nil
	})
}

// Count reports the total size and item counts behind a link.
func (c *Cloner) Count(sourceLink string) (*CountResult, error) {
	return withAuthFallback(c, func() (*CountResult, error) {
		return c.count(sourceLink)
	})
}

func (c *Cloner) count(sourceLink string) (*CountResult, error) {
	meta, err := c.getFileMetadata(sourceLink)
	if err != nil {
		return nil, err
	}
	if meta.Trashed {
		return nil, newError("FILE DOESN'T EXIST")
	}
	meta = c.resolveShortcut(meta, false)

	if meta.MimeType == FolderMIME {
		totalBytes, totalFiles, totalFolders, err := c.scanFolderStats(meta.Id)
		if err != nil {
			return nil, err
		}
		return &CountResult{
			ID:       meta.Id,
			Name:     nameOrUnnamed(meta),
			MIMEType: FolderMIME,
			Size:     totalBytes,
			Files:    totalFiles,
			Folders:  totalFolders,
		}, nil
	}

	mimeType := meta.MimeType
	if mimeType == "" {
		mimeType = FileMIME
	}
	return &CountResult{
		ID:       meta.Id,
		Name:     nameOrUnnamed(meta),
		MIMEType: mimeType,
		Size:     itemSize(meta),
		Files:    1,
		Folders:  0,
	}, nil
}

func nameOrUnnamed(f *drivev3.File) string {
	if f.Name == "" {
		return "Unnamed"
	}
	return f.Name
}

// PrepareClone validates source and destination and detects duplicates before
// any copying starts, so the bot can fail fast with a clear message.
func (c *Cloner) PrepareClone(sourceLink, destinationID string) (*Precheck, error) {
	return withAuthFallback(c, func() (*Precheck, error) {
		return c.prepareClone(sourceLink, destinationID)
	})
}

func (c *Cloner) prepareClone(sourceLink, destinationID string) (*Precheck, error) {
	parsed, err := ParseLink(sourceLink)
	if err != nil {
		return nil, err
	}

	srcMeta, err := c.filesGetWithFallback(parsed.FileID, metaFields, parsed.ResourceKey)
	if err != nil {
		switch httpStatus(err) {
		case 404:
			return nil, newError("FILE NOT FOUND OR NOT SHARED")
		case 403:
			return nil, newError("YOU DON'T HAVE PERMS")
		}
		return nil, normalizeHTTPError(err)
	}

	if srcMeta.Trashed {
		return nil, newError("FILE DOESN'T EXIST")
	}
	srcMeta = c.resolveShortcut(srcMeta, false)
	if srcMeta.Trashed {
		return nil, newError("FILE DOESN'T EXIST")
	}

	if err := c.checkDestination(destinationID, "id,name,mimeType,trashed,capabilities(canAddChildren,canEdit)"); err != nil {
		return nil, err
	}

	duplicate, err := c.findChildByName(destinationID, nameOrUnnamed(srcMeta))
	if err != nil {
		return nil, normalizeHTTPError(err)
	}
	if duplicate != nil {
		return nil, newDuplicateError(Existing{
			ID:   duplicate.Id,
			Name: duplicate.Name,
			URL:  driveURLFor(duplicate),
		})
	}

	return &Precheck{
		Name:           nameOrUnnamed(srcMeta),
		SourceID:       srcMeta.Id,
		SourceMIMEType: srcMeta.MimeType,
		AuthMode:       c.AuthMode,
	}, nil
}

// checkDestination verifies the destination is a writable, live folder.
func (c *Cloner) checkDestination(destinationID, fields string) error {
	dstMeta, err := c.filesGet(destinationID, fields, "")
	if err != nil {
		if status := httpStatus(err); status == 403 || status == 404 {
			return newError("YOU DON'T HAVE PERMS")
		}
		return normalizeHTTPError(err)
	}
	if dstMeta.Trashed || dstMeta.MimeType != FolderMIME {
		return newError("YOU DON'T HAVE PERMS")
	}
	caps := dstMeta.Capabilities
	if caps == nil || (!caps.CanAddChildren && !caps.CanEdit) {
		return newError("YOU DON'T HAVE PERMS")
	}
	return nil
}

// filesCopy performs a server-side copy, rotating identities when the active
// one hits a per-account quota.
func (c *Cloner) filesCopy(sourceID, name, parentID, resourceKey string) (*drivev3.File, error) {
	for {
		copied, err := retryDo(c.ctx, func() (*drivev3.File, error) {
			call := c.svc.Files.
				Copy(sourceID, &drivev3.File{Name: name, Parents: []string{parentID}}).
				SupportsAllDrives(true).
				Fields("id,name,size,webViewLink,mimeType").
				Context(c.ctx)
			applyResourceKey(call.Header(), sourceID, resourceKey)
			return call.Do()
		})
		if err == nil {
			return copied, nil
		}
		if !isCopyAuthRotationError(err) || !c.switchToNextAuth() {
			return nil, err
		}
	}
}

func (c *Cloner) createFolder(name, parentID string) (*drivev3.File, error) {
	return retryDo(c.ctx, func() (*drivev3.File, error) {
		return c.svc.Files.Create(&drivev3.File{
			Name:     name,
			MimeType: FolderMIME,
			Parents:  []string{parentID},
		}).
			SupportsAllDrives(true).
			Fields("id,name,webViewLink").
			Context(c.ctx).
			Do()
	})
}

func (c *Cloner) listChildren(folderID, itemType string) ([]*drivev3.File, error) {
	queryParts := []string{fmt.Sprintf("'%s' in parents", escapeQueryValue(folderID)), "trashed=false"}
	switch itemType {
	case "files":
		queryParts = append(queryParts, fmt.Sprintf("mimeType != '%s'", FolderMIME))
	case "folders":
		queryParts = append(queryParts, fmt.Sprintf("mimeType = '%s'", FolderMIME))
	}
	query := strings.Join(queryParts, " and ")

	var children []*drivev3.File
	pageToken := ""
	for {
		resp, err := retryDo(c.ctx, func() (*drivev3.FileList, error) {
			return c.svc.Files.List().
				Q(query).
				Fields("nextPageToken, files(id,name,size,mimeType,quotaBytesUsed,resourceKey,shortcutDetails)").
				SupportsAllDrives(true).
				IncludeItemsFromAllDrives(true).
				PageToken(pageToken).
				PageSize(1000).
				OrderBy("folder, name").
				Context(c.ctx).
				Do()
		})
		if err != nil {
			return nil, err
		}
		children = append(children, resp.Files...)
		pageToken = resp.NextPageToken
		if pageToken == "" {
			return children, nil
		}
	}
}

func (c *Cloner) findChildByName(parentID, name string) (*drivev3.File, error) {
	query := fmt.Sprintf("'%s' in parents and trashed=false and name='%s'",
		escapeQueryValue(parentID), escapeQueryValue(name))
	resp, err := retryDo(c.ctx, func() (*drivev3.FileList, error) {
		return c.svc.Files.List().
			Q(query).
			Fields("files(id,name,mimeType,webViewLink)").
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true).
			PageSize(1).
			Context(c.ctx).
			Do()
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Files) == 0 {
		return nil, nil
	}
	return resp.Files[0], nil
}

// scanFolderStats walks a folder tree to total up bytes and item counts.
func (c *Cloner) scanFolderStats(folderID string) (totalBytes, totalFiles, totalFolders int64, err error) {
	stack := []string{folderID}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		children, err := c.listChildren(current, "all")
		if err != nil {
			return 0, 0, 0, err
		}
		for _, child := range children {
			child = c.resolveShortcut(child, false)
			if child.MimeType == FolderMIME {
				totalFolders++
				stack = append(stack, child.Id)
				continue
			}
			totalFiles++
			totalBytes += itemSize(child)
		}
	}
	return totalBytes, totalFiles, totalFolders, nil
}

func (c *Cloner) copyFolderRecursive(
	sourceFolderID, destinationFolderID string,
	prog *progress.Clone,
	progressCB ProgressFunc,
) error {
	children, err := c.listChildren(sourceFolderID, "all")
	if err != nil {
		return err
	}

	for _, child := range children {
		child = c.resolveShortcut(child, false)
		if child.MimeType == FolderMIME {
			newFolder, err := c.createFolder(child.Name, destinationFolderID)
			if err != nil {
				return err
			}
			prog.Status = "Entering folder: " + child.Name
			progressCB(prog, false)
			if err := c.copyFolderRecursive(child.Id, newFolder.Id, prog, progressCB); err != nil {
				return err
			}
			continue
		}

		prog.Status = "Copying file: " + child.Name
		progressCB(prog, false)
		if _, err := c.filesCopy(child.Id, child.Name, destinationFolderID, child.ResourceKey); err != nil {
			return err
		}
		prog.CopiedFiles++
		prog.CopiedBytes += itemSize(child)
		progressCB(prog, false)
	}
	return nil
}

// resolveShortcut follows a shortcut to its target, returning the original item
// when the target is missing or trashed.
func (c *Cloner) resolveShortcut(item *drivev3.File, tryAllAuth bool) *drivev3.File {
	if item.MimeType != ShortcutMIME || item.ShortcutDetails == nil {
		return item
	}
	targetID := item.ShortcutDetails.TargetId
	if targetID == "" {
		return item
	}
	target := c.shortcutTargetMetadata(targetID, tryAllAuth)
	if target == nil || target.Trashed {
		return item
	}
	return target
}

func (c *Cloner) shortcutTargetMetadata(targetID string, tryAllAuth bool) *drivev3.File {
	originalAuth := c.AuthMode
	originalService := c.svc

	authModes := []string{c.AuthMode}
	if tryAllAuth {
		authModes = c.authOrder
	}

	for _, authMode := range authModes {
		svc, err := c.getService(authMode)
		if err != nil {
			continue
		}
		c.AuthMode = authMode
		c.svc = svc

		target, err := c.filesGetWithFallback(targetID, metaFields, "")
		if err == nil {
			c.AuthMode = originalAuth
			c.svc = originalService
			return target
		}
	}

	c.AuthMode = originalAuth
	c.svc = originalService
	return nil
}

// Clone copies a file or folder tree into the destination folder.
func (c *Cloner) Clone(
	sourceLink, destinationID string,
	prog *progress.Clone,
	progressCB ProgressFunc,
) (*Result, error) {
	if c.preferred != "" {
		if err := c.useAuth(); err != nil {
			return nil, err
		}
		return c.clone(sourceLink, destinationID, prog, progressCB)
	}
	return withAuthFallback(c, func() (*Result, error) {
		return c.clone(sourceLink, destinationID, prog, progressCB)
	})
}

func (c *Cloner) clone(
	sourceLink, destinationID string,
	prog *progress.Clone,
	progressCB ProgressFunc,
) (*Result, error) {
	parsed, err := ParseLink(sourceLink)
	if err != nil {
		return nil, err
	}

	srcMeta, err := c.filesGetWithFallback(parsed.FileID, metaFields, parsed.ResourceKey)
	if err != nil {
		return nil, normalizeHTTPError(err)
	}
	if srcMeta.Trashed {
		return nil, newError("FILE DOESN'T EXIST")
	}
	srcMeta = c.resolveShortcut(srcMeta, false)
	if srcMeta.Trashed {
		return nil, newError("FILE DOESN'T EXIST")
	}

	// Re-check the destination at clone time to avoid races since PrepareClone.
	if err := c.checkDestination(destinationID, "id,mimeType,trashed,capabilities(canAddChildren,canEdit)"); err != nil {
		return nil, err
	}

	duplicate, err := c.findChildByName(destinationID, nameOrUnnamed(srcMeta))
	if err != nil {
		return nil, normalizeHTTPError(err)
	}
	if duplicate != nil {
		return nil, newDuplicateError(Existing{
			ID:   duplicate.Id,
			Name: duplicate.Name,
			URL:  driveURLFor(duplicate),
		})
	}

	prog.TaskName = nameOrUnnamed(srcMeta)

	if srcMeta.MimeType == FolderMIME {
		prog.Status = "Scanning folder for total size"
		progressCB(prog, true)

		totalBytes, totalFiles, _, err := c.scanFolderStats(srcMeta.Id)
		if err != nil {
			return nil, normalizeHTTPError(err)
		}
		prog.TotalBytes = totalBytes
		prog.TotalFiles = totalFiles

		topFolder, err := c.createFolder(srcMeta.Name, destinationID)
		if err != nil {
			return nil, normalizeHTTPError(err)
		}
		prog.Status = "Starting server-side folder copy"
		progressCB(prog, true)

		if err := c.copyFolderRecursive(srcMeta.Id, topFolder.Id, prog, progressCB); err != nil {
			return nil, normalizeHTTPError(err)
		}

		prog.Status = "Finalizing"
		progressCB(prog, true)

		url := topFolder.WebViewLink
		if url == "" {
			url = "https://drive.google.com/drive/folders/" + topFolder.Id
		}
		return &Result{ID: topFolder.Id, Name: topFolder.Name, MIMEType: FolderMIME, URL: url}, nil
	}

	size := itemSize(srcMeta)
	prog.TotalBytes = size
	prog.TotalFiles = 1
	prog.Status = "Copying file: " + srcMeta.Name
	progressCB(prog, true)

	resourceKey := srcMeta.ResourceKey
	if resourceKey == "" {
		resourceKey = parsed.ResourceKey
	}
	copied, err := c.filesCopy(srcMeta.Id, srcMeta.Name, destinationID, resourceKey)
	if err != nil {
		return nil, normalizeHTTPError(err)
	}

	prog.CopiedFiles = 1
	prog.CopiedBytes = size
	prog.Status = "Finalizing"
	progressCB(prog, true)

	mimeType := copied.MimeType
	if mimeType == "" {
		mimeType = "unknown"
	}
	url := copied.WebViewLink
	if url == "" {
		url = "https://drive.google.com/file/d/" + copied.Id + "/view"
	}
	return &Result{ID: copied.Id, Name: copied.Name, MIMEType: mimeType, URL: url}, nil
}

// Delete resolves a link and removes the item in one step.
func (c *Cloner) Delete(sourceLink string) (*DeleteResult, error) {
	target, err := c.PrepareDelete(sourceLink)
	if err != nil {
		return nil, err
	}
	return c.PerformDelete(target)
}

// PrepareDelete validates the target and decides between delete and trash.
func (c *Cloner) PrepareDelete(sourceLink string) (*DeleteTarget, error) {
	return withAuthFallback(c, func() (*DeleteTarget, error) {
		return c.prepareDelete(sourceLink)
	})
}

func (c *Cloner) prepareDelete(sourceLink string) (*DeleteTarget, error) {
	parsed, err := ParseLink(sourceLink)
	if err != nil {
		return nil, err
	}

	srcMeta, err := c.filesGetWithFallback(
		parsed.FileID,
		"id,name,mimeType,webViewLink,capabilities(canDelete,canTrash),trashed",
		parsed.ResourceKey,
	)
	if err != nil {
		if httpStatus(err) == 404 {
			return nil, newError("FILE NOT FOUND OR NOT SHARED")
		}
		return nil, normalizeHTTPError(err)
	}
	if srcMeta.Trashed {
		return nil, newError("FILE DOESN'T EXIST")
	}

	caps := srcMeta.Capabilities
	mode := ""
	switch {
	case caps != nil && caps.CanDelete:
		mode = "delete"
	case caps != nil && caps.CanTrash:
		mode = "trash"
	default:
		return nil, newError("YOU DON'T HAVE PERMS")
	}

	return &DeleteTarget{
		ID:       srcMeta.Id,
		Name:     nameOrUnnamed(srcMeta),
		MIMEType: srcMeta.MimeType,
		Mode:     mode,
		AuthMode: c.AuthMode,
	}, nil
}

// PerformDelete removes a previously prepared target under its own identity.
func (c *Cloner) PerformDelete(target *DeleteTarget) (*DeleteResult, error) {
	if err := c.useAuth(); err != nil {
		return nil, err
	}

	var action string
	var err error
	if target.Mode == "delete" {
		_, err = retryDo(c.ctx, func() (struct{}, error) {
			return struct{}{}, c.svc.Files.Delete(target.ID).
				SupportsAllDrives(true).
				Context(c.ctx).
				Do()
		})
		action = "deleted"
	} else {
		_, err = retryDo(c.ctx, func() (*drivev3.File, error) {
			return c.svc.Files.Update(target.ID, &drivev3.File{
				Trashed:         true,
				ForceSendFields: []string{"Trashed"},
			}).
				SupportsAllDrives(true).
				Fields("id,trashed").
				Context(c.ctx).
				Do()
		})
		action = "trashed"
	}
	if err != nil {
		switch httpStatus(err) {
		case 404:
			return nil, newError("FILE NOT FOUND OR NOT SHARED")
		case 403:
			return nil, newError("YOU DON'T HAVE PERMS")
		}
		return nil, normalizeHTTPError(err)
	}

	itemType := "file"
	if target.MIMEType == FolderMIME {
		itemType = "folder"
	}
	name := target.Name
	if name == "" {
		name = "Unnamed"
	}
	return &DeleteResult{ID: target.ID, Name: name, Type: itemType, Action: action}, nil
}
