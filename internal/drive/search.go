package drive

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	drivev3 "google.golang.org/api/drive/v3"
)

// SearchItem is one search hit, enriched with a resolved size and the identity
// that found it.
type SearchItem struct {
	File *drivev3.File

	// ComputedSize is nil when no size could be determined.
	ComputedSize    *int64
	ComputedFiles   int64
	ComputedFolders int64

	URL       string
	AuthMode  string
	DriveID   string
	DriveName string
}

// ID returns the Drive ID of the hit, following resolved shortcuts.
func (s *SearchItem) ID() string { return s.File.Id }

// Name returns the display name of the hit.
func (s *SearchItem) Name() string { return nameOrUnnamed(s.File) }

// Size returns the best known size and whether one is known at all.
func (s *SearchItem) Size() (int64, bool) {
	if s.ComputedSize != nil {
		return *s.ComputedSize, true
	}
	if size := itemSize(s.File); size > 0 {
		return size, true
	}
	return 0, false
}

func (s *SearchItem) sortSize() int64 {
	size, _ := s.Size()
	return size
}

// Search looks for items by name across every Shared Drive visible to each
// configured identity. It does not search My Drive.
func (c *Cloner) Search(query string, limit int, itemType string) ([]*SearchItem, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, newError("Search query is required")
	}
	switch itemType {
	case "files", "folders", "all":
	default:
		return nil, newError("Invalid search type")
	}

	perAuthLimit := limit
	if perAuthLimit < 1 {
		perAuthLimit = 1
	}

	// Identities have separate Drive quotas and nothing to say to each other,
	// so they search at the same time.
	outcomes := make([]searchOutcome, len(c.authOrder))
	var wg sync.WaitGroup
	for idx, authMode := range c.authOrder {
		wg.Add(1)
		go func(idx int, authMode string) {
			defer wg.Done()
			outcomes[idx] = c.searchAs(authMode, query, perAuthLimit, itemType)
		}(idx, authMode)
	}
	wg.Wait()

	var results []*SearchItem
	seenIDs := map[string]bool{}
	var lastErr error

	// Merged in configured order, so the earliest identity still owns a hit
	// that several of them can see.
	for _, outcome := range outcomes {
		if outcome.err != nil {
			lastErr = outcome.err
			continue
		}
		for _, item := range outcome.items {
			if item.ID() == "" || seenIDs[item.ID()] {
				continue
			}
			// Resolving a shortcut can change the ID, so re-check afterwards.
			outcome.cloner.attachSearchSize(item)
			if item.ID() == "" || seenIDs[item.ID()] {
				continue
			}
			seenIDs[item.ID()] = true
			item.URL = driveURLFor(item.File)
			item.AuthMode = outcome.authMode
			results = append(results, item)
		}
	}

	if len(results) > 0 {
		sort.SliceStable(results, func(i, j int) bool {
			return results[i].sortSize() > results[j].sortSize()
		})
		if len(results) > limit {
			results = results[:limit]
		}
		return results, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

// searchOutcome is what one identity found, along with the Cloner that found
// it: sizes and shortcuts have to be resolved as the same identity.
type searchOutcome struct {
	authMode string
	cloner   *Cloner
	items    []*SearchItem
	err      error
}

// searchAs runs a search as one identity on a Cloner of its own, so identities
// can run concurrently without sharing the mutable auth state.
func (c *Cloner) searchAs(authMode, query string, limit int, itemType string) searchOutcome {
	sub, err := New(c.ctx, c.cfg, authMode)
	if err != nil {
		return searchOutcome{authMode: authMode, err: err}
	}

	svc, err := sub.getService(authMode)
	if err != nil {
		return searchOutcome{authMode: authMode, err: authErrorFor(err)}
	}
	sub.AuthMode, sub.svc = authMode, svc

	items, err := sub.searchAllSharedDrives(query, limit, itemType)
	if err != nil {
		return searchOutcome{authMode: authMode, err: normalizeHTTPError(err)}
	}
	return searchOutcome{authMode: authMode, cloner: sub, items: items}
}

// searchFields is the metadata projection used for search hits.
const searchFields = "files(id,name,size,mimeType,webViewLink,modifiedTime,driveId," +
	"quotaBytesUsed,resourceKey,shortcutDetails)"

// searchDriveWorkers bounds the per-drive queries one identity has in flight.
const searchDriveWorkers = 8

// buildSearchQuery renders the Drive query for a search. Every term is escaped,
// so a title like "Marvel's Spider-Man 2" is matched rather than rejected.
func buildSearchQuery(query, itemType string) string {
	var queryTerms []string
	for _, term := range strings.Fields(query) {
		queryTerms = append(queryTerms, escapeQueryValue(term))
	}
	if len(queryTerms) == 0 {
		queryTerms = []string{escapeQueryValue(query)}
	}

	queryParts := make([]string, 0, len(queryTerms)+2)
	for _, term := range queryTerms {
		queryParts = append(queryParts, fmt.Sprintf("name contains '%s'", term))
	}
	queryParts = append(queryParts, "trashed=false")
	switch itemType {
	case "files":
		queryParts = append(queryParts, fmt.Sprintf("mimeType != '%s'", FolderMIME))
	case "folders":
		queryParts = append(queryParts, fmt.Sprintf("mimeType = '%s'", FolderMIME))
	}
	return strings.Join(queryParts, " and ")
}

func (c *Cloner) searchAllSharedDrives(query string, limit int, itemType string) ([]*SearchItem, error) {
	q := buildSearchQuery(query, itemType)

	drives, err := c.listSharedDrives()
	if err != nil {
		return nil, err
	}

	// Drive has no way to query several Shared Drives at once with this sort, so
	// each drive is its own request. Running them one at a time was the whole
	// cost of a search: an account with 30 drives paid 30 round trips.
	perDrive := make([][]*SearchItem, len(drives))
	errs := make([]error, len(drives))

	var wg sync.WaitGroup
	slots := make(chan struct{}, searchDriveWorkers)
	for idx, sharedDrive := range drives {
		wg.Add(1)
		go func(idx int, sharedDrive *drivev3.Drive) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			items, err := c.searchSharedDrive(sharedDrive, q, limit)
			perDrive[idx], errs[idx] = items, err
		}(idx, sharedDrive)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	var results []*SearchItem
	for _, items := range perDrive {
		results = append(results, items...)
	}
	return results, nil
}

func (c *Cloner) searchSharedDrive(
	sharedDrive *drivev3.Drive,
	q string,
	limit int,
) ([]*SearchItem, error) {
	list := func(orderBy string) (*drivev3.FileList, error) {
		return retryDo(c.ctx, func() (*drivev3.FileList, error) {
			call := c.svc.Files.List().
				Q(q).
				Fields(searchFields).
				Corpora("drive").
				DriveId(sharedDrive.Id).
				SupportsAllDrives(true).
				IncludeItemsFromAllDrives(true).
				PageSize(int64(limit)).
				Context(c.ctx)
			if orderBy != "" {
				call = call.OrderBy(orderBy)
			}
			return call.Do()
		})
	}

	resp, err := list("quotaBytesUsed desc")
	if err != nil {
		// Some drives reject that sort key; retry unsorted before failing.
		if httpStatus(err) != 400 {
			return nil, err
		}
		resp, err = list("")
		if err != nil {
			return nil, err
		}
	}

	items := make([]*SearchItem, 0, len(resp.Files))
	for _, file := range resp.Files {
		driveID := file.DriveId
		if driveID == "" {
			driveID = sharedDrive.Id
		}
		items = append(items, &SearchItem{
			File:      file,
			DriveID:   driveID,
			DriveName: sharedDrive.Name,
		})
	}
	return items, nil
}

func (c *Cloner) listSharedDrives() ([]*drivev3.Drive, error) {
	var drives []*drivev3.Drive
	pageToken := ""
	for {
		resp, err := retryDo(c.ctx, func() (*drivev3.DriveList, error) {
			return c.svc.Drives.List().
				PageSize(100).
				PageToken(pageToken).
				Fields("nextPageToken, drives(id,name)").
				Context(c.ctx).
				Do()
		})
		if err != nil {
			return nil, err
		}
		drives = append(drives, resp.Drives...)
		pageToken = resp.NextPageToken
		if pageToken == "" {
			return drives, nil
		}
	}
}

// attachSearchSize resolves shortcuts and works out a usable size for a hit,
// scanning folder trees when Drive reports no size of its own.
func (c *Cloner) attachSearchSize(item *SearchItem) {
	file := item.File

	if file.MimeType == ShortcutMIME {
		target := c.resolveShortcut(file, true)
		if target != file {
			item.File = target
			size := itemSize(target)
			item.ComputedSize = &size
			return
		}
		// The target is unreachable: point at it anyway, but claim no size.
		if file.ShortcutDetails != nil && file.ShortcutDetails.TargetId != "" {
			file.Id = file.ShortcutDetails.TargetId
			if file.ShortcutDetails.TargetMimeType != "" {
				file.MimeType = file.ShortcutDetails.TargetMimeType
			}
		}
		file.Size = 0
		file.QuotaBytesUsed = 0
		item.ComputedSize = nil
		return
	}

	if size := itemSize(file); size > 0 {
		item.ComputedSize = &size
		return
	}

	if file.MimeType != FolderMIME {
		// Listing sometimes omits sizes that a direct metadata read reports.
		if refreshed := c.refreshSearchItemMetadata(file); refreshed != nil {
			if size := itemSize(refreshed); size > 0 {
				item.ComputedSize = &size
			}
		}
		return
	}

	// Drive never reports recursive folder sizes, so total them up ourselves.
	totalBytes, totalFiles, totalFolders, err := c.scanFolderStats(file.Id)
	if err != nil {
		return
	}
	item.ComputedSize = &totalBytes
	item.ComputedFiles = totalFiles
	item.ComputedFolders = totalFolders
}

func (c *Cloner) refreshSearchItemMetadata(file *drivev3.File) *drivev3.File {
	if file.Id == "" {
		return nil
	}
	refreshed, err := c.filesGetWithFallback(file.Id, metaFields, file.ResourceKey)
	if err != nil {
		return nil
	}
	return refreshed
}
