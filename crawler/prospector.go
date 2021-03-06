package crawler

import (
	"os"
	"path/filepath"
	"time"

	cfg "github.com/elastic/filebeat/config"
	"github.com/elastic/filebeat/harvester"
	"github.com/elastic/filebeat/input"
	"github.com/elastic/libbeat/logp"
)

type Prospector struct {
	ProspectorConfig cfg.ProspectorConfig
	prospectorList   map[string]ProspectorFileStat
	iteration        uint32
	lastscan         time.Time
	registrar        *Registrar
	missingFiles     map[string]os.FileInfo
	running          bool
}

// Contains statistic about file when it was last seend by the prospector
type ProspectorFileStat struct {
	Fileinfo      os.FileInfo /* the file info */
	Harvester     chan int64  /* the harvester will send an event with its offset when it closes */
	LastIteration uint32      /* int number of the last iterations in which we saw this file */
}

// Init sets up default config for prospector
func (p *Prospector) Init() error {

	err := p.setupProspectorConfig()
	if err != nil {
		return err
	}

	err = p.setupHarvesterConfig()
	if err != nil {
		return err
	}

	return nil

}

// Setup Prospector Config
func (p *Prospector) setupProspectorConfig() error {
	var err error
	config := &p.ProspectorConfig

	config.IgnoreOlderDuration, err = getConfigDuration(config.IgnoreOlder, cfg.DefaultIgnoreOlderDuration, "ignore_older")
	if err != nil {
		return err
	}

	config.ScanFrequencyDuration, err = getConfigDuration(config.ScanFrequency, cfg.DefaultScanFrequency, "scan_frequency")
	if err != nil {
		return err
	}

	// Init File Stat list
	p.prospectorList = make(map[string]ProspectorFileStat)

	return nil
}

// Setup Harvester Config
func (p *Prospector) setupHarvesterConfig() error {

	var err error
	config := &p.ProspectorConfig.Harvester

	// Setup Buffer Size
	if config.BufferSize == 0 {
		config.BufferSize = cfg.DefaultHarvesterBufferSize
	}

	// Setup DocumentType
	if config.DocumentType == "" {
		config.DocumentType = cfg.DefaultDocumentType
	}

	// Setup InputType
	if config.InputType == "" {
		config.InputType = cfg.DefaultInputType
	}

	config.BackoffDuration, err = getConfigDuration(config.Backoff, cfg.DefaultBackoff, "backoff")
	if err != nil {
		return err
	}

	// Setup DocumentType
	if config.BackoffFactor == 0 {
		config.BackoffFactor = cfg.DefaultBackoffFactor
	}

	config.MaxBackoffDuration, err = getConfigDuration(config.MaxBackoff, cfg.DefaultMaxBackoff, "max_backoff")
	if err != nil {
		return err
	}

	config.PartialLineWaitingDuration, err = getConfigDuration(config.PartialLineWaiting, cfg.DefaultPartialLineWaiting, "partial_line_waiting")
	if err != nil {
		return err
	}

	return nil
}

// getConfigDuration builds the duration based on the input string.
// Returns error if an invalid string duration is passed
// In case no duration is set, default duration will be used.
func getConfigDuration(config string, duration time.Duration, name string) (time.Duration, error) {

	// Setup Ignore Older
	if config != "" {
		var err error
		duration, err = time.ParseDuration(config)
		if err != nil {
			logp.Warn("Failed to parse %s value '%s'. Error was: %s\n", name, config)
			return 0, err
		}
	}
	logp.Debug("prospector", "Set %s duration to %s", name, duration)

	return duration, nil
}

// Starts scanning through all the file paths and fetch the related files. Start a harvester for each file
func (p *Prospector) Run(spoolChan chan *input.FileEvent) {

	p.running = true

	// Handle any "-" (stdin) paths
	for i, path := range p.ProspectorConfig.Paths {

		logp.Debug("prospector", "Harvest path: %s", path)

		if path == "-" {
			// Offset and Initial never get used when path is "-"
			h, err := harvester.NewHarvester(
				p.ProspectorConfig, &p.ProspectorConfig.Harvester,
				path, nil, spoolChan)
			if err != nil {
				logp.Err("Error initializing harvester: %v", err)
				return
			}

			h.Start()

			// Remove it from the file list
			p.ProspectorConfig.Paths = append(p.ProspectorConfig.Paths[:i], p.ProspectorConfig.Paths[i+1:]...)
		}
	}

	// Seed last scan time
	p.lastscan = time.Now()

	// Now let's do one quick scan to pick up new files
	for _, path := range p.ProspectorConfig.Paths {
		p.scan(path, spoolChan)
	}

	// This signals we finished considering the previous state
	event := &input.FileState{
		Source: nil,
	}
	p.registrar.Persist <- event

	for {
		newlastscan := time.Now()

		for _, path := range p.ProspectorConfig.Paths {
			// Scan - flag false so new files always start at beginning TODO: is this still working as expected?
			p.scan(path, spoolChan)
		}

		p.lastscan = newlastscan

		// Defer next scan for the defined scanFrequency
		time.Sleep(p.ProspectorConfig.ScanFrequencyDuration)
		logp.Debug("prospector", "Start next scan")

		// Clear out files that disappeared and we've stopped harvesting
		for file, lastinfo := range p.prospectorList {
			if len(lastinfo.Harvester) != 0 && lastinfo.LastIteration < p.iteration {
				delete(p.prospectorList, file)
			}
		}

		p.iteration++ // Overflow is allowed

		if !p.running {
			break
		}
	}
}

// Scans the specific path which can be a glob (/**/**/*.log)
// For all found files it is checked if a harvester should be started
func (p *Prospector) scan(path string, output chan *input.FileEvent) {

	logp.Debug("prospector", "scan path %s", path)
	// Evaluate the path as a wildcards/shell glob
	matches, err := filepath.Glob(path)
	if err != nil {
		logp.Debug("prospector", "glob(%s) failed: %v", path, err)
		return
	}

	p.missingFiles = map[string]os.FileInfo{}

	// Check any matched files to see if we need to start a harvester
	for _, file := range matches {
		logp.Debug("prospector", "Check file for harvesting: %s", file)

		// Stat the file, following any symlinks.
		fileinfo, err := os.Stat(file)

		// TODO(sissel): check err
		if err != nil {
			logp.Debug("prospector", "stat(%s) failed: %s", file, err)
			continue
		}

		newFile := input.File{
			FileInfo: fileinfo,
		}

		if newFile.FileInfo.IsDir() {
			logp.Debug("prospector", "Skipping directory: %s", file)
			continue
		}

		// Check the current info against p.prospectorinfo[file]
		lastinfo, isKnown := p.prospectorList[file]

		oldFile := input.File{
			FileInfo: lastinfo.Fileinfo,
		}

		// Create a new prospector info with the stat info for comparison
		newInfo := ProspectorFileStat{
			Fileinfo:      newFile.FileInfo,
			Harvester:     make(chan int64, 1),
			LastIteration: p.iteration,
		}

		// Conditions for starting a new harvester:
		// - file path hasn't been seen before
		// - the file's inode or device changed
		if !isKnown {
			p.checkNewFile(&newInfo, file, output)
		} else {
			newInfo.Harvester = lastinfo.Harvester
			p.checkExistingFile(&newInfo, &newFile, &oldFile, file, output)
		}

		// Track the stat data for this file for later comparison to check for
		// rotation/etc
		p.prospectorList[file] = newInfo
	} // for each file matched by the glob
}

// Check if harvester for new file has to be started
// For a new file the following options exist:
func (p *Prospector) checkNewFile(newinfo *ProspectorFileStat, file string, output chan *input.FileEvent) {

	logp.Debug("prospector", "Start harvesting unknown file: %s", file)

	// Init harvester with info
	h, err := harvester.NewHarvester(
		p.ProspectorConfig, &p.ProspectorConfig.Harvester,
		file, newinfo.Harvester, output)
	if err != nil {
		logp.Err("Error initializing harvester: %v", err)
		return
	}

	// Check for unmodified time, but only if the file modification time is before the last scan started
	// This ensures we don't skip genuine creations with dead times less than 10s
	if newinfo.Fileinfo.ModTime().Before(p.lastscan) &&
		time.Since(newinfo.Fileinfo.ModTime()) > p.ProspectorConfig.IgnoreOlderDuration {

		// Call crawler if there if there exists a state for the given file
		offset, resuming := p.registrar.fetchState(file, newinfo.Fileinfo)

		// Are we resuming a dead file? We have to resume even if dead so we catch any old updates to the file
		// This is safe as the harvester, once it hits the EOF and a timeout, will stop harvesting
		// Once we detect changes again we can resume another harvester again - this keeps number of go routines to a minimum
		if resuming {
			logp.Debug("prospector", "Resuming harvester on a previously harvested file: %s", file)

			h.Offset = offset
			h.Start()
		} else {
			// Old file, skip it, but push offset of file size so we start from the end if this file changes and needs picking up
			logp.Debug("prospector", "Skipping file (older than ignore older of %v): %s", p.ProspectorConfig.IgnoreOlderDuration, file)
			newinfo.Harvester <- newinfo.Fileinfo.Size()
		}
	} else if previousFile := p.getPreviousFile(file, newinfo.Fileinfo); previousFile != "" {
		// This file was simply renamed (known inode+dev) - link the same harvester channel as the old file
		logp.Debug("prospector", "File rename was detected: %s -> %s", previousFile, file)
		newinfo.Harvester = p.prospectorList[previousFile].Harvester

	} else {

		// Call crawler if there if there exists a state for the given file
		offset, resuming := p.registrar.fetchState(file, newinfo.Fileinfo)

		// Are we resuming a file or is this a completely new file?
		if resuming {
			logp.Debug("prospector", "Resuming harvester on a previously harvested file: %s", file)
		} else {
			logp.Debug("prospector", "Launching harvester on new file: %s", file)
		}

		// Launch the harvester
		h.Offset = offset
		h.Start()
	}
}

// checkExistingFile checks if a harvester has to be started for a already known file
// For existing files the following options exist:
// * Last reading position is 0, no harvester has to be started as old harvester probably still busy
// * The old known modification time is older then the current one. Start at last known position
// * The new file is not the same as the old file, means file was renamed
// ** New file is actually really a new file, start a new harvester
// ** Renamed file has a state, continue there
func (p *Prospector) checkExistingFile(newinfo *ProspectorFileStat, newFile *input.File, oldFile *input.File, file string, output chan *input.FileEvent) {

	logp.Debug("prospector", "Update existing file for harvesting: %s", file)

	h, err := harvester.NewHarvester(
		p.ProspectorConfig, &p.ProspectorConfig.Harvester,
		file, newinfo.Harvester, output)
	if err != nil {
		logp.Err("Error initializing harvester: %v", err)
		return
	}

	if !oldFile.IsSameFile(newFile) {

		if previousFile := p.getPreviousFile(file, newinfo.Fileinfo); previousFile != "" {
			// This file was renamed from another file we know - link the same harvester channel as the old file
			logp.Debug("prospector", "File rename was detected: %s -> %s", previousFile, file)
			logp.Debug("prospector", "Launching harvester on renamed file: %s", file)

			newinfo.Harvester = p.prospectorList[previousFile].Harvester
		} else {
			// File is not the same file we saw previously, it must have rotated and is a new file
			logp.Debug("prospector", "Launching harvester on rotated file: %s", file)

			// Forget about the previous harvester and let it continue on the old file - so start a new channel to use with the new harvester
			newinfo.Harvester = make(chan int64, 1)

			// Start a new harvester on the path
			h.Start()
		}

		// Keep the old file in missingFiles so we don't rescan it if it was renamed and we've not yet reached the new filename
		// We only need to keep it for the remainder of this iteration then we can assume it was deleted and forget about it
		p.missingFiles[file] = oldFile.FileInfo

	} else if len(newinfo.Harvester) != 0 && oldFile.FileInfo.ModTime() != newinfo.Fileinfo.ModTime() {
		// Resume harvesting of an old file we've stopped harvesting from
		logp.Debug("prospector", "Resuming harvester on an old file that was just modified: %s", file)

		// Start a harvester on the path; an old file was just modified and it doesn't have a harvester
		// The offset to continue from will be stored in the harvester channel - so take that to use and also clear the channel
		h.Offset = <-newinfo.Harvester
		h.Start()
	} else {
		logp.Debug("prospector", "Not harvesting, file didn't change: %s", file)
	}
}

func (p *Prospector) Stop() {
	// TODO: Stopping is currently not implemented
}

// Check if the given file was renamed. If file is known but with different path,
// renamed will be set true and previous will be set to the previously known file path.
// Otherwise renamed will be false.
func (p *Prospector) getPreviousFile(file string, info os.FileInfo) string {
	// TODO: To implement this properly the file state of the previous file is required.

	for path, pFileStat := range p.prospectorList {
		if path == file {
			continue
		}

		if os.SameFile(info, pFileStat.Fileinfo) {
			return path
		}
	}

	// Now check the missingfiles
	for path, fileInfo := range p.missingFiles {

		if os.SameFile(info, fileInfo) {
			return path
		}
	}

	// NOTE(ruflin): should instead an error be returned if not previous file?
	return ""
}
