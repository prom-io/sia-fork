package renter

import (
	"fmt"
	"os"
	"testing"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/modules/renter/filesystem"
	"gitlab.com/NebulousLabs/Sia/modules/renter/siafile"
	"gitlab.com/NebulousLabs/Sia/persist"
	"gitlab.com/NebulousLabs/Sia/siatest/dependencies"
)

// TestBuildUnfinishedChunks probes buildUnfinishedChunks to make sure that the
// correct chunks are being added to the heap
func TestBuildUnfinishedChunks(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create Renter
	rt, err := newRenterTesterWithDependency(t.Name(), &dependencies.DependencyDisableRepairAndHealthLoops{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Create file on disk
	path, err := rt.createZeroByteFileOnDisk()
	if err != nil {
		t.Fatal(err)
	}
	// Create file with more than 1 chunk and mark the first chunk at stuck
	rsc, _ := siafile.NewRSCode(1, 1)
	siaPath, err := modules.NewSiaPath("stuckFile")
	if err != nil {
		t.Fatal(err)
	}
	up := modules.FileUploadParams{
		Source:      path,
		SiaPath:     siaPath,
		ErasureCode: rsc,
	}
	err = rt.renter.staticFileSystem.NewSiaFile(up.SiaPath, up.Source, up.ErasureCode, crypto.GenerateSiaKey(crypto.RandomCipherType()), 10e3, persist.DefaultDiskPermissionsTest, false)
	if err != nil {
		t.Fatal(err)
	}
	f, err := rt.renter.staticFileSystem.OpenSiaFile(up.SiaPath)
	if err != nil {
		t.Fatal(err)
	}
	if f.NumChunks() <= 1 {
		t.Fatalf("File created with not enough chunks for test, have %v need at least 2", f.NumChunks())
	}
	if err = f.SetStuck(uint64(0), true); err != nil {
		t.Fatal(err)
	}

	// Create maps to pass into methods
	hosts := make(map[string]struct{})
	offline := make(map[string]bool)
	goodForRenew := make(map[string]bool)

	// Manually add workers to worker pool
	for i := 0; i < int(f.NumChunks()); i++ {
		rt.renter.staticWorkerPool.workers[string(i)] = &worker{
			killChan: make(chan struct{}),
		}
	}

	// Call managedBuildUnfinishedChunks as not stuck loop, all un stuck chunks
	// should be returned
	uucs := rt.renter.managedBuildUnfinishedChunks(f, hosts, targetUnstuckChunks, offline, goodForRenew)
	if len(uucs) != int(f.NumChunks())-1 {
		t.Fatalf("Incorrect number of chunks returned, expected %v got %v", int(f.NumChunks())-1, len(uucs))
	}
	for _, c := range uucs {
		if c.stuck {
			t.Fatal("Found stuck chunk when expecting only unstuck chunks")
		}
	}

	// Call managedBuildUnfinishedChunks as stuck loop, all stuck chunks should
	// be returned
	uucs = rt.renter.managedBuildUnfinishedChunks(f, hosts, targetStuckChunks, offline, goodForRenew)
	if len(uucs) != 1 {
		t.Fatalf("Incorrect number of chunks returned, expected 1 got %v", len(uucs))
	}
	for _, c := range uucs {
		if !c.stuck {
			t.Fatal("Found unstuck chunk when expecting only stuck chunks")
		}
	}

	// Remove file on disk to make file not repairable
	err = os.Remove(path)
	if err != nil {
		t.Fatal(err)
	}

	// Call managedBuildUnfinishedChunks as not stuck loop, since the file is
	// now not repairable it should return no chunks
	uucs = rt.renter.managedBuildUnfinishedChunks(f, hosts, targetUnstuckChunks, offline, goodForRenew)
	if len(uucs) != 0 {
		t.Fatalf("Incorrect number of chunks returned, expected 0 got %v", len(uucs))
	}

	// Call managedBuildUnfinishedChunks as stuck loop, all chunks should be
	// returned because they should have been marked as stuck by the previous
	// call and stuck chunks should still be returned if the file is not
	// repairable
	uucs = rt.renter.managedBuildUnfinishedChunks(f, hosts, targetStuckChunks, offline, goodForRenew)
	if len(uucs) != int(f.NumChunks()) {
		t.Fatalf("Incorrect number of chunks returned, expected %v got %v", f.NumChunks(), len(uucs))
	}
	for _, c := range uucs {
		if !c.stuck {
			t.Fatal("Found unstuck chunk when expecting only stuck chunks")
		}
	}
}

// TestBuildChunkHeap probes managedBuildChunkHeap to make sure that the correct
// chunks are being added to the heap
func TestBuildChunkHeap(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create Renter
	rt, err := newRenterTesterWithDependency(t.Name(), &dependencies.DependencyDisableRepairAndHealthLoops{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Create 2 files
	source, err := rt.createZeroByteFileOnDisk()
	if err != nil {
		t.Fatal(err)
	}
	rsc, _ := siafile.NewRSCode(1, 1)
	up := modules.FileUploadParams{
		Source:      source,
		SiaPath:     modules.RandomSiaPath(),
		ErasureCode: rsc,
	}
	err = rt.renter.staticFileSystem.NewSiaFile(up.SiaPath, up.Source, up.ErasureCode, crypto.GenerateSiaKey(crypto.RandomCipherType()), 10e3, persist.DefaultDiskPermissionsTest, false)
	if err != nil {
		t.Fatal(err)
	}
	f1, err := rt.renter.staticFileSystem.OpenSiaFile(up.SiaPath)
	if err != nil {
		t.Fatal(err)
	}

	// Manually add workers to worker pool and create host map
	hosts := make(map[string]struct{})
	for i := 0; i < int(f1.NumChunks()); i++ {
		rt.renter.staticWorkerPool.workers[string(i)] = &worker{
			killChan: make(chan struct{}),
		}
	}

	// Call managedBuildChunkHeap as repair loop, we should see all the chunks
	// from the file added
	rt.renter.managedBuildChunkHeap(modules.RootSiaPath(), hosts, targetUnstuckChunks)
	if rt.renter.uploadHeap.managedLen() != int(f1.NumChunks()) {
		t.Fatalf("Expected heap length of %v but got %v", f1.NumChunks(), rt.renter.uploadHeap.managedLen())
	}
}

// addChunksOfDifferentHealth is a helper function for TestUploadHeap to add
// numChunks number of chunks that each have different healths to the uploadHeap
func addChunksOfDifferentHealth(r *Renter, numChunks int, stuck, fileRecentlySuccessful, priority bool) error {
	var UID siafile.SiafileUID
	if priority {
		UID = "priority"
	} else if fileRecentlySuccessful {
		UID = "fileRecentlySuccessful"
	} else if stuck {
		UID = "stuck"
	} else {
		UID = "unstuck"
	}

	// Add numChunks number of chunks to the upload heap. Set the id index and
	// health to the value of health. Since health of 0 is full health, start i
	// at 1
	for i := 1; i <= numChunks; i++ {
		chunk := &unfinishedUploadChunk{
			id: uploadChunkID{
				fileUID: UID,
				index:   uint64(i),
			},
			stuck:                  stuck,
			fileRecentlySuccessful: fileRecentlySuccessful,
			priority:               priority,
			health:                 float64(i),
			availableChan:          make(chan struct{}),
		}
		if !r.uploadHeap.managedPush(chunk) {
			return fmt.Errorf("unable to push chunk: %v", chunk)
		}
	}
	return nil
}

// TestUploadHeap probes the upload heap to make sure chunks are sorted
// correctly
func TestUploadHeap(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create renter
	rt, err := newRenterTesterWithDependency(t.Name(), &dependencies.DependencyDisableRepairAndHealthLoops{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Add chunks to heap. Chunks are prioritize by stuck status first and then
	// by piecesComplete/piecesNeeded
	//
	// Add 2 chunks of each type to confirm the type and the health is
	// prioritized properly
	err = addChunksOfDifferentHealth(rt.renter, 2, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	err = addChunksOfDifferentHealth(rt.renter, 2, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	err = addChunksOfDifferentHealth(rt.renter, 2, false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	err = addChunksOfDifferentHealth(rt.renter, 2, false, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// There should be 8 chunks in the heap
	if rt.renter.uploadHeap.managedLen() != 8 {
		t.Fatalf("Expected %v chunks in heap found %v",
			8, rt.renter.uploadHeap.managedLen())
	}

	// Check order of chunks
	//  - First 2 chunks should be priority
	//  - Second 2 chunks should be fileRecentlyRepair
	//  - Third 2 chunks should be stuck
	//  - Last 2 chunks should be unstuck
	chunk1 := rt.renter.uploadHeap.managedPop()
	chunk2 := rt.renter.uploadHeap.managedPop()
	if !chunk1.priority || !chunk2.priority {
		t.Fatalf("Expected chunks to be priority, got priority %v and %v",
			chunk1.priority, chunk2.priority)
	}
	if chunk1.health < chunk2.health {
		t.Fatalf("expected top chunk to have worst health, chunk1: %v, chunk2: %v",
			chunk1.health, chunk2.health)
	}
	chunk1 = rt.renter.uploadHeap.managedPop()
	chunk2 = rt.renter.uploadHeap.managedPop()
	if !chunk1.fileRecentlySuccessful || !chunk2.fileRecentlySuccessful {
		t.Fatalf("Expected chunks to be fileRecentlySuccessful, got fileRecentlySuccessful %v and %v",
			chunk1.fileRecentlySuccessful, chunk2.fileRecentlySuccessful)
	}
	if chunk1.health < chunk2.health {
		t.Fatalf("expected top chunk to have worst health, chunk1: %v, chunk2: %v",
			chunk1.health, chunk2.health)
	}
	chunk1 = rt.renter.uploadHeap.managedPop()
	chunk2 = rt.renter.uploadHeap.managedPop()
	if !chunk1.stuck || !chunk2.stuck {
		t.Fatalf("Expected chunks to be stuck, got stuck %v and %v",
			chunk1.stuck, chunk2.stuck)
	}
	if chunk1.health < chunk2.health {
		t.Fatalf("expected top chunk to have worst health, chunk1: %v, chunk2: %v",
			chunk1.health, chunk2.health)
	}
	chunk1 = rt.renter.uploadHeap.managedPop()
	chunk2 = rt.renter.uploadHeap.managedPop()
	if chunk1.health < chunk2.health {
		t.Fatalf("expected top chunk to have worst health, chunk1: %v, chunk2: %v",
			chunk1.health, chunk2.health)
	}
}

// TestAddChunksToHeap probes the managedAddChunksToHeap method to ensure it is
// functioning as intended
func TestAddChunksToHeap(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create Renter
	rt, err := newRenterTesterWithDependency(t.Name(), &dependencies.DependencyDisableRepairAndHealthLoops{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Create File params
	_, rsc := testingFileParams()
	source, err := rt.createZeroByteFileOnDisk()
	if err != nil {
		t.Fatal(err)
	}
	up := modules.FileUploadParams{
		Source:      source,
		ErasureCode: rsc,
	}

	// Create files in multiple directories
	var numChunks uint64
	var dirSiaPaths []modules.SiaPath
	names := []string{"rootFile", "subdir/File", "subdir2/file"}
	for _, name := range names {
		siaPath, err := modules.NewSiaPath(name)
		if err != nil {
			t.Fatal(err)
		}
		up.SiaPath = siaPath
		err = rt.renter.staticFileSystem.NewSiaFile(up.SiaPath, up.Source, up.ErasureCode, crypto.GenerateSiaKey(crypto.RandomCipherType()), modules.SectorSize, persist.DefaultDiskPermissionsTest, false)
		if err != nil {
			t.Fatal(err)
		}
		f, err := rt.renter.staticFileSystem.OpenSiaFile(up.SiaPath)
		if err != nil {
			t.Fatal(err)
		}
		// Track number of chunks
		numChunks += f.NumChunks()
		dirSiaPath, err := siaPath.Dir()
		if err != nil {
			t.Fatal(err)
		}
		// Make sure directories are created
		err = rt.renter.CreateDir(dirSiaPath, modules.DefaultDirPerm)
		if err != nil && err != filesystem.ErrExists {
			t.Fatal(err)
		}
		dirSiaPaths = append(dirSiaPaths, dirSiaPath)
	}

	// Call bubbled to ensure directory metadata is updated
	for _, siaPath := range dirSiaPaths {
		err := rt.renter.managedBubbleMetadata(siaPath)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Manually add workers to worker pool and create host map
	hosts := make(map[string]struct{})
	for i := 0; i < rsc.MinPieces(); i++ {
		rt.renter.staticWorkerPool.workers[string(i)] = &worker{
			killChan: make(chan struct{}),
		}
	}

	// Make sure directory Heap is ready
	err = rt.renter.managedPushUnexploredDirectory(modules.RootSiaPath())
	if err != nil {
		t.Fatal(err)
	}

	// call managedAddChunksTo Heap
	siaPaths, err := rt.renter.managedAddChunksToHeap(hosts)
	if err != nil {
		t.Fatal(err)
	}

	// Confirm that all chunks from all the directories were added since there
	// are not enough chunks in only one directory to fill the heap
	if len(siaPaths) != 3 {
		t.Fatal("Expected 3 siaPaths to be returned, got", siaPaths)
	}
	if rt.renter.uploadHeap.managedLen() != int(numChunks) {
		t.Fatalf("Expected uploadHeap to have %v chunks but it has %v chunks", numChunks, rt.renter.uploadHeap.managedLen())
	}
}

// TestAddDirectoryBackToHeap ensures that when not all the chunks in a
// directory are added to the uploadHeap that the directory is added back to the
// directoryHeap with an updated Health
func TestAddDirectoryBackToHeap(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create Renter with interrupt dependency
	rt, err := newRenterTesterWithDependency(t.Name(), &dependencies.DependencyDisableRepairAndHealthLoops{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Create file
	rsc, _ := siafile.NewRSCode(1, 1)
	siaPath, err := modules.NewSiaPath("test")
	if err != nil {
		t.Fatal(err)
	}
	source, err := rt.createZeroByteFileOnDisk()
	if err != nil {
		t.Fatal(err)
	}
	up := modules.FileUploadParams{
		Source:      source,
		SiaPath:     siaPath,
		ErasureCode: rsc,
	}
	err = rt.renter.staticFileSystem.NewSiaFile(up.SiaPath, up.Source, up.ErasureCode, crypto.GenerateSiaKey(crypto.RandomCipherType()), modules.SectorSize, persist.DefaultDiskPermissionsTest, false)
	if err != nil {
		t.Fatal(err)
	}
	f, err := rt.renter.staticFileSystem.OpenSiaFile(up.SiaPath)
	if err != nil {
		t.Fatal(err)
	}

	// Create maps for method inputs
	hosts := make(map[string]struct{})
	offline := make(map[string]bool)
	goodForRenew := make(map[string]bool)

	// Manually add workers to worker pool
	for i := 0; i < int(f.NumChunks()); i++ {
		rt.renter.staticWorkerPool.mu.Lock()
		rt.renter.staticWorkerPool.workers[string(i)] = &worker{
			killChan: make(chan struct{}),
		}
		rt.renter.staticWorkerPool.mu.Unlock()
	}

	// Confirm we are starting with an empty upload and directory heap
	if rt.renter.uploadHeap.managedLen() != 0 {
		t.Fatal("Expected upload heap to be empty but has length of", rt.renter.uploadHeap.managedLen())
	}
	// "Empty" -> gets initialized with the root dir, therefore should have one
	// directory in it.
	if rt.renter.directoryHeap.managedLen() != 1 {
		t.Fatal("Expected directory heap to be empty but has length of", rt.renter.directoryHeap.managedLen())
	}
	// Reset the dir heap to clear the root dir out, rest of test wants an empty
	// heap.
	rt.renter.directoryHeap.managedReset()

	// Add chunks from file to uploadHeap
	rt.renter.callBuildAndPushChunks([]*filesystem.FileNode{f}, hosts, targetUnstuckChunks, offline, goodForRenew)

	// Upload heap should now have NumChunks chunks and directory heap should still be empty
	if rt.renter.uploadHeap.managedLen() != int(f.NumChunks()) {
		t.Fatalf("Expected upload heap to be of size %v but was %v", f.NumChunks(), rt.renter.uploadHeap.managedLen())
	}
	if rt.renter.directoryHeap.managedLen() != 0 {
		t.Fatal("Expected directory heap to be empty but has length of", rt.renter.directoryHeap.managedLen())
	}

	// Empty uploadHeap
	rt.renter.uploadHeap.managedReset()

	// Fill upload heap with chunks that are a worse health than the chunks in
	// the file
	var i uint64
	for rt.renter.uploadHeap.managedLen() < maxUploadHeapChunks {
		chunk := &unfinishedUploadChunk{
			id: uploadChunkID{
				fileUID: "chunk",
				index:   i,
			},
			stuck:           false,
			piecesCompleted: -1,
			piecesNeeded:    1,
			availableChan:   make(chan struct{}),
		}
		if !rt.renter.uploadHeap.managedPush(chunk) {
			t.Fatal("Chunk should have been added to heap")
		}
		i++
	}

	// Record length of upload heap
	uploadHeapLen := rt.renter.uploadHeap.managedLen()

	// Try and add chunks to upload heap again
	rt.renter.callBuildAndPushChunks([]*filesystem.FileNode{f}, hosts, targetUnstuckChunks, offline, goodForRenew)

	// No chunks should have been added to the upload heap
	if rt.renter.uploadHeap.managedLen() != uploadHeapLen {
		t.Fatalf("Expected upload heap to be of size %v but was %v", uploadHeapLen, rt.renter.uploadHeap.managedLen())
	}
	// There should be one directory in the directory heap now
	if rt.renter.directoryHeap.managedLen() != 1 {
		t.Fatal("Expected directory heap to have 1 element but has length of", rt.renter.directoryHeap.managedLen())
	}
	// The directory should be marked as explored
	d := rt.renter.directoryHeap.managedPop()
	if !d.explored {
		t.Fatal("Directory should be explored")
	}
	// The directory should be the root directory as that is where we created
	// the test file
	if !d.siaPath.Equals(modules.RootSiaPath()) {
		t.Fatal("Expected Directory siapath to be the root siaPath but was", d.siaPath.String())
	}
	// The directory health should be that of the file since none of the chunks
	// were added
	health, _, _, _, _ := f.Health(offline, goodForRenew)
	if d.health != health {
		t.Fatalf("Expected directory health to be %v but was %v", health, d.health)
	}
}

// TestUploadHeapMaps tests that the uploadHeap's maps are properly updated
// through pushing, popping, and reseting the heap
func TestUploadHeapMaps(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create renter
	rt, err := newRenterTesterWithDependency(t.Name(), &dependencies.DependencyDisableRepairAndHealthLoops{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Add stuck and unstuck chunks to heap to fill up the heap maps
	numHeapChunks := uint64(10)
	sf, err := rt.renter.newRenterTestFile()
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(0); i < numHeapChunks; i++ {
		// Create minimum chunk
		stuck := i%2 == 0
		chunk := &unfinishedUploadChunk{
			id: uploadChunkID{
				fileUID: siafile.SiafileUID(fmt.Sprintf("chunk - %v", i)),
				index:   i,
			},
			fileEntry:       sf.Copy(),
			stuck:           stuck,
			piecesCompleted: 1,
			piecesNeeded:    1,
			availableChan:   make(chan struct{}),
		}
		// push chunk to heap
		if !rt.renter.uploadHeap.managedPush(chunk) {
			t.Fatal("unable to push chunk", chunk)
		}
		// Confirm chunk is in the correct map
		if stuck {
			_, ok := rt.renter.uploadHeap.stuckHeapChunks[chunk.id]
			if !ok {
				t.Fatal("stuck chunk not in stuck chunk heap map")
			}
		} else {
			_, ok := rt.renter.uploadHeap.unstuckHeapChunks[chunk.id]
			if !ok {
				t.Fatal("unstuck chunk not in unstuck chunk heap map")
			}
		}
	}

	// Close original siafile entry
	sf.Close()

	// Confirm length of maps
	if len(rt.renter.uploadHeap.unstuckHeapChunks) != int(numHeapChunks/2) {
		t.Fatalf("Expected %v unstuck chunks in map but found %v", numHeapChunks/2, len(rt.renter.uploadHeap.unstuckHeapChunks))
	}
	if len(rt.renter.uploadHeap.stuckHeapChunks) != int(numHeapChunks/2) {
		t.Fatalf("Expected %v stuck chunks in map but found %v", numHeapChunks/2, len(rt.renter.uploadHeap.stuckHeapChunks))
	}
	if len(rt.renter.uploadHeap.repairingChunks) != 0 {
		t.Fatalf("Expected %v repairing chunks in map but found %v", 0, len(rt.renter.uploadHeap.repairingChunks))
	}

	// Pop off some chunks
	poppedChunks := 3
	for i := 0; i < poppedChunks; i++ {
		// Pop chunk
		chunk := rt.renter.uploadHeap.managedPop()
		// Confirm it is in the repairing map
		_, ok := rt.renter.uploadHeap.repairingChunks[chunk.id]
		if !ok {
			t.Fatal("popped chunk not found in repairing map")
		}
		// Confirm the chunk cannot be pushed back onto the heap
		if rt.renter.uploadHeap.managedPush(chunk) {
			t.Fatal("should not have been able to push chunk back onto heap")
		}
	}

	// Confirm length of maps
	if len(rt.renter.uploadHeap.repairingChunks) != poppedChunks {
		t.Fatalf("Expected %v repairing chunks in map but found %v", poppedChunks, len(rt.renter.uploadHeap.repairingChunks))
	}
	remainingChunks := len(rt.renter.uploadHeap.unstuckHeapChunks) + len(rt.renter.uploadHeap.stuckHeapChunks)
	if remainingChunks != int(numHeapChunks)-poppedChunks {
		t.Fatalf("Expected %v chunks to still be in the heap maps but found %v", int(numHeapChunks)-poppedChunks, remainingChunks)
	}

	// Reset the heap
	if err := rt.renter.uploadHeap.managedReset(); err != nil {
		t.Fatal(err)
	}

	// Confirm length of maps
	if len(rt.renter.uploadHeap.repairingChunks) != poppedChunks {
		t.Fatalf("Expected %v repairing chunks in map but found %v", poppedChunks, len(rt.renter.uploadHeap.repairingChunks))
	}
	remainingChunks = len(rt.renter.uploadHeap.unstuckHeapChunks) + len(rt.renter.uploadHeap.stuckHeapChunks)
	if remainingChunks != 0 {
		t.Fatalf("Expected %v chunks to still be in the heap maps but found %v", 0, remainingChunks)
	}
}

// TestUploadHeapPauseChan makes sure that sequential calls to pause and resume
// won't cause panics for closing a closed channel
func TestUploadHeapPauseChan(t *testing.T) {
	// Initial UploadHeap with the pauseChan initialized such that the uploads
	// and repairs are not paused
	uh := uploadHeap{
		pauseChan: make(chan struct{}),
	}
	close(uh.pauseChan)
	if uh.managedIsPaused() {
		t.Error("Repairs and Uploads should not be paused")
	}

	// Call resume on an initialized heap
	uh.managedResume()

	// Call Pause twice in a row
	uh.managedPause(DefaultPauseDuration)
	uh.managedPause(DefaultPauseDuration)
	// Call Resume twice in a row
	uh.managedResume()
	uh.managedResume()
}
