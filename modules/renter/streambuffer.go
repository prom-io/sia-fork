package renter

// NOTE: This stream buffer is uninfished in a couple of ways. The first way is
// that it's not possible to cancel fetches. The second way is that fetches are
// not prioritized, there should be a higher priority on data that is closer to
// the current stream offset. The third is that the amount of data which gets
// fetched is not dynamically adjusted. The streamer really should be monitoring
// the total amount of time it takes for a call to the data source to return
// some data, and should buffer accordingly. If auto-adjusting the lookahead
// size, care needs to be taken to ensure not to exceed the
// bytesBufferedPerStream size, as exceeding that will cause issues with the
// lru, and cause data fetches to be evicted before they become useful.

import (
	"io"
	"sync"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/errors"
)

const (
	// minimumDataSections is set to two because the streamer always tries to
	// buffer at least the current data section and the next data section for
	// the current offset of a stream.
	//
	// Three as a number was considered so that in addition to buffering one
	// piece ahead, a previous piece could also be cached. This was considered
	// to be less valuable than keeping memory requirements low -
	// minimumDataSections is only at play if there is not enough room for
	// multiple cache nodes in the bytesBufferedPerStream.
	minimumDataSections = 2
)

var (
	// bytesBufferedPerStream is the total amount of data that gets allocated
	// per stream. If the RequestSize of a stream buffer is less than three
	// times the bytesBufferedPerStream, that much data will be allocated
	// instead.
	//
	// For example, if the RequestSize is 10kb and the bytesBufferedPerStream is
	// 100kb, then each stream is going to buffer 10 segments that are each 10kb
	// long in the LRU.
	//
	// But if the RequestSize is 50kb and the bytesBufferedPerStream is 100kb,
	// then each stream is going to buffer 3 segments that are each 50kb long in
	// the LRU, for a total of 150kb.
	bytesBufferedPerStream = build.Select(build.Var{
		Dev:      uint64(1 << 25), // 32 MiB
		Standard: uint64(1 << 25), // 32 MiB
		Testing:  uint64(1 << 8),  // 256 bytes
	}).(uint64)
)

// streamBufferDataSource is an interface that the stream buffer uses to fetch
// data. This type is internal to the renter as there are plans to expand on the
// type.
type streamBufferDataSource interface {
	// DataSize should return the size of the data. When the streamBuffer is
	// reading from the data source, it will ensure that none of the read calls
	// go beyond the boundary of the data source.
	DataSize() uint64

	// ID returns the ID of the data source. This should be unique to the data
	// source - that is, every data source that returns the same ID should have
	// identical data and be fully interchangeable.
	ID() streamDataSourceID

	// RequestSize should return the request size that the dataSource expects
	// the streamBuffer to use. The streamBuffer will always make ReadAt calls
	// that are of the suggested request size and byte aligned.
	//
	// If the request size is small, many ReadAt calls will be made in parallel.
	// If the dataSource can handle high parallelism, a smaller request size
	// should be recommended to the streamBuffer, because that will reduce
	// latency. If the dataSource cannot handle high parallelism, a larger
	// request size should be used to optimize for total throughput.
	//
	// A general rule of thumb is that the streamer should be able to
	// comfortably handle 100 mbps (high end 4K video) if the user's local
	// connection has that much throughput.
	RequestSize() uint64

	// SilentClose is an io.Closer that does not return an error. The data
	// source is expected to handle any logging or reporting that is necessary
	// if the closing fails.
	SilentClose()

	// ReaderAt allows the stream buffer to request specific data chunks.
	io.ReaderAt
}

// streamDataSourceID is a type safe crypto.Hash which is used to uniquely
// identify data sources for streams.
type streamDataSourceID crypto.Hash

// dataSection represents a section of data from a data source. The data section
// includes a refcount of how many different streams have the data in their LRU.
// If the refCount is ever set to 0, the data section should be deleted. Because
// the dataSection has no mutex, the refCount falls under the consistency domain
// of the object holding it, which should always be a streamBuffer.
type dataSection struct {
	// dataAvailable, externData, and externErr work together. The data and
	// error are not allowed to be accessed by external threads until the data
	// available channel has been closed. Once the dataAvailable channel has
	// been closed, externData and externErr are to be treated like static
	// fields.
	dataAvailable chan struct{}
	externData    []byte
	externErr     error

	refCount uint64
}

// stream is a single stream that uses a stream buffer. The stream implements
// io.ReadSeeker and io.Closer, and must be closed when it is done being used.
// The stream will cache data, both data that has been accessed recently as well
// as data that is in front of the current read head. The stream buffer is a
// common cache that is used between all streams that are using the same data
// source, allowing each stream to depend on the other streams if data has
// already been loaded.
type stream struct {
	lru    *leastRecentlyUsedCache
	offset uint64

	mu                 sync.Mutex
	staticStreamBuffer *streamBuffer
}

// streamBuffer is a buffer for a single dataSource.
type streamBuffer struct {
	dataSections map[uint64]*dataSection

	// externRefCount is in the same consistency domain as the streamBufferSet,
	// it needs to be incremented and decremented simultaneously with the
	// creation and deletion of the streamBuffer.
	externRefCount uint64

	mu                    sync.Mutex
	staticDataSize        uint64
	staticDataSource      streamBufferDataSource
	staticDataSectionSize uint64
	staticStreamBufferSet *streamBufferSet
	staticStreamID        streamDataSourceID
}

// streamBufferSet tracks all of the stream buffers that are currently active.
// When a new stream is created, the stream buffer set is referenced to check
// whether another stream using the same data source already exists.
type streamBufferSet struct {
	streams map[streamDataSourceID]*streamBuffer

	mu sync.Mutex
}

// newStreamBufferSet initializes and returns a stream buffer set.
func newStreamBufferSet() *streamBufferSet {
	return &streamBufferSet{
		streams: make(map[streamDataSourceID]*streamBuffer),
	}
}

// callNewStream will create a stream that implements io.Close and
// io.ReadSeeker. A dataSource must be provided for the stream so that the
// stream can fetch data in advance of calls to 'Read' and attempt to provide a
// smooth streaming experience.
//
// The 'sourceID' is a unique identifier for the dataSource which allows
// multiple streams fetching data from the same source to combine their cache.
// This shared cache only comes into play if the streams are simultaneously
// accessing the same data, allowing the buffer to save on memory and access
// latency.
//
// Each stream has a separate LRU for determining what data to buffer. Because
// the LRU is distinct to the stream, the shared cache feature will not result
// in one stream evicting data from another stream's LRU.
func (sbs *streamBufferSet) callNewStream(dataSource streamBufferDataSource, initialOffset uint64) *stream {
	// Grab the streamBuffer for the provided sourceID. If no streamBuffer for
	// the sourceID exists, create a new one.
	sourceID := dataSource.ID()
	sbs.mu.Lock()
	streamBuf, exists := sbs.streams[sourceID]
	if !exists {
		streamBuf = &streamBuffer{
			dataSections: make(map[uint64]*dataSection),

			staticDataSize:        dataSource.DataSize(),
			staticDataSource:      dataSource,
			staticDataSectionSize: dataSource.RequestSize(),
			staticStreamBufferSet: sbs,
			staticStreamID:        sourceID,
		}
		sbs.streams[sourceID] = streamBuf
	} else {
		// Another data source already exists for this content which will be
		// used instead of the input data source. Close the input source.
		dataSource.SilentClose()
	}
	streamBuf.externRefCount++
	sbs.mu.Unlock()

	// Determine how many data sections the stream should cache.
	dataSectionsToCache := bytesBufferedPerStream / streamBuf.staticDataSectionSize
	if dataSectionsToCache < minimumDataSections {
		dataSectionsToCache = minimumDataSections
	}

	// Create a stream that points to the stream buffer.
	stream := &stream{
		lru:    newLeastRecentlyUsedCache(dataSectionsToCache, streamBuf),
		offset: initialOffset,

		staticStreamBuffer: streamBuf,
	}
	stream.prepareOffset()
	return stream
}

// managedData will block until the data for a data section is available, and
// then return the data. The data is not safe to modify.
func (ds *dataSection) managedData() ([]byte, error) {
	<-ds.dataAvailable
	return ds.externData, ds.externErr
}

// Close will release all of the resources held by a stream.
func (s *stream) Close() error {
	// Drop all nodes from the lru.
	s.lru.callEvictAll()

	// Remove the stream from the streamBuffer.
	streamBuf := s.staticStreamBuffer
	streamBufSet := streamBuf.staticStreamBufferSet
	streamBufSet.managedRemoveStream(streamBuf)
	return nil
}

// Read will read data into 'b', returning the number of bytes read and any
// errors. Read will not fill 'b' up all the way if only part of the data is
// available.
func (s *stream) Read(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Convenience variables.
	dataSize := s.staticStreamBuffer.staticDataSize
	dataSectionSize := s.staticStreamBuffer.staticDataSectionSize
	sb := s.staticStreamBuffer

	// Check for EOF.
	if s.offset == dataSize {
		return 0, io.EOF
	}

	// Get the index of the current section and the offset within the current
	// section.
	currentSection := s.offset / dataSectionSize
	offsetInSection := s.offset % dataSectionSize

	// Determine how many bytes are remaining within the current section, this
	// forms an upper bound on how many bytes can be read.
	var bytesRemaining uint64
	lastSection := (currentSection+1)*dataSectionSize >= dataSize
	if !lastSection {
		bytesRemaining = dataSectionSize - offsetInSection
	} else {
		bytesRemaining = dataSize - s.offset
	}

	// Determine how many bytes should be read.
	var bytesToRead uint64
	if bytesRemaining > uint64(len(b)) {
		bytesToRead = uint64(len(b))
	} else {
		bytesToRead = bytesRemaining
	}

	// Fetch the dataSection that has the data we want to read.
	sb.mu.Lock()
	dataSection, exists := sb.dataSections[currentSection]
	sb.mu.Unlock()
	if !exists {
		build.Critical("data section should always in the stream buffer for the current offset of a stream")
	}

	// Block until the data is available.
	data, err := dataSection.managedData()
	if err != nil {
		return 0, errors.AddContext(err, "read call failed because data section fetch failed")
	}
	// Copy the data into the read request.
	n := copy(b, data[offsetInSection:offsetInSection+bytesToRead])
	s.offset += uint64(n)

	// Send the call to prepare the next data section.
	s.prepareOffset()
	return n, nil
}

// Seek will move the read head of the stream to the provided offset.
func (s *stream) Seek(offset int64, whence int) (int64, error) {
	// Input checking.
	if offset < 0 {
		return int64(s.offset), errors.New("offset cannot be negative in call to seek")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update the offset of the stream according to the inputs.
	dataSize := s.staticStreamBuffer.staticDataSize
	switch whence {
	case io.SeekStart:
		s.offset = uint64(offset)
	case io.SeekCurrent:
		newOffset := s.offset + uint64(offset)
		if newOffset > dataSize {
			return int64(s.offset), errors.New("offset cannot seek beyond the bounds of the file")
		}
		s.offset = newOffset
	case io.SeekEnd:
		if uint64(offset) > dataSize {
			return int64(s.offset), errors.New("cannot seek before the front of the file")
		}
		s.offset = dataSize - uint64(offset)
	default:
		return int64(s.offset), errors.New("invalid value for 'whence' in call to seek")
	}

	// Prepare the fetch of the updated offset.
	s.prepareOffset()
	return int64(s.offset), nil
}

// prepareOffset will ensure that the dataSection containing the offset is made
// available in the LRU, and that the following dataSection is also available.
func (s *stream) prepareOffset() {
	// Convenience variables.
	dataSize := s.staticStreamBuffer.staticDataSize
	dataSectionSize := s.staticStreamBuffer.staticDataSectionSize

	// If the offset is already at the end of the data, there is nothing to do.
	if s.offset == dataSize {
		return
	}

	// Update the current data section. The update call will trigger the
	// streamBuffer to fetch the dataSection if the dataSection is not already
	// in the streamBuffer cache.
	index := s.offset / dataSectionSize
	s.lru.callUpdate(index)

	// If there is a following data section, update that as well.
	nextIndex := index + 1
	if nextIndex*dataSectionSize < dataSize {
		s.lru.callUpdate(nextIndex)
	}
}

// callFetchDataSection will increment the refcount of a dataSection in the
// stream buffer. If the dataSection is not currently available in the stream
// buffer, the data section will be fetched from the dataSource.
func (sb *streamBuffer) callFetchDataSection(index uint64) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	// Fetch the relevant dataSection, creating a new one if necessary.
	dataSection, exists := sb.dataSections[index]
	if !exists {
		dataSection = sb.newDataSection(index)
	}
	// Increment the refcount of the dataSection.
	dataSection.refCount++
}

// callRemoveDataSection will decrement the refcount of a data section in the
// stream buffer. If the refcount reaches zero, the data section will be deleted
// from the stream buffer.
func (sb *streamBuffer) callRemoveDataSection(index uint64) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	// Fetch the data section.
	dataSection, exists := sb.dataSections[index]
	if !exists {
		build.Critical("remove called on data section that does not exist")
	}
	// Decrement the refcount.
	dataSection.refCount--
	// Delete the data section if the refcount has fallen to zero.
	if dataSection.refCount == 0 {
		delete(sb.dataSections, index)
	}
}

// newDataSection will create a new data section for the streamBuffer and spin
// up a goroutine to pull the data from the data source.
func (sb *streamBuffer) newDataSection(index uint64) *dataSection {
	// Convenience variables.
	dataSize := sb.staticDataSize
	dataSectionSize := sb.staticDataSectionSize

	// Determine the fetch size for the data section. The fetch size should be
	// equal to the dataSectionSize unless this is the final section, in which
	// case the section size should be exactly big enough to request all
	// remaining bytes.
	var fetchSize uint64
	if (index+1)*dataSectionSize > dataSize {
		fetchSize = dataSize - (index * dataSectionSize)
	} else {
		fetchSize = dataSectionSize
	}

	// Create the data section, allocating the right number of bytes for the
	// ReadAt call to fill out.
	ds := &dataSection{
		dataAvailable: make(chan struct{}),
		externData:    make([]byte, fetchSize),
	}
	sb.dataSections[index] = ds

	// Perform the data fetch in a goroutine. The dataAvailable channel will be
	// closed when the data is available.
	go func() {
		_, err := sb.staticDataSource.ReadAt(ds.externData, int64(index*dataSectionSize))
		if err != nil {
			ds.externErr = errors.AddContext(err, "data section fetch failed")
		}
		close(ds.dataAvailable)
	}()
	return ds
}

// managedRemoveStream will remove a stream from a stream buffer. If the total
// number of streams using that stream buffer reaches zero, the stream buffer
// will be removed from the stream buffer set.
//
// The reference counter for a stream buffer needs to be in the domain of the
// stream buffer set because the stream buffer needs to be deleted from the
// stream buffer set simultaneously with the reference counter reaching zero.
func (sbs *streamBufferSet) managedRemoveStream(sb *streamBuffer) {
	sbs.mu.Lock()
	defer sbs.mu.Unlock()

	// Decrement the refcount of the streamBuffer.
	sb.externRefCount--
	if sb.externRefCount > 0 {
		// streamBuffer still in use, nothing to do.
		return
	}

	// Close out the streamBuffer and its data source.
	delete(sbs.streams, sb.staticStreamID)
	sb.staticDataSource.SilentClose()
}
