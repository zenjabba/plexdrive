package chunk

import (
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"sync"

	"time"

	. "github.com/claudetech/loggo/default"
)

const RAM = 1
const DISK = 2

type Storage struct {
	ChunkPath   string
	ChunkSize   int64
	MaxTempSize int64
	queue       chan *Item
	chunks      map[string][]byte
	chunksLock  sync.Mutex
	toc         map[string]*ChunkInfo
	tocLock     sync.Mutex
}

type ChunkInfo struct {
	location int
	access   time.Time
}

type Item struct {
	id    string
	bytes []byte
}

func NewStorage(chunkPath string, chunkSize, maxTempSize int64) *Storage {
	storage := Storage{
		ChunkPath:   chunkPath,
		ChunkSize:   chunkSize,
		MaxTempSize: maxTempSize,
		queue:       make(chan *Item, 100),
		chunks:      make(map[string][]byte),
		toc:         make(map[string]*ChunkInfo),
	}

	go storage.thread()

	return &storage
}

func (s *Storage) Clear() error {
	if err := os.RemoveAll(s.ChunkPath); nil != err {
		return fmt.Errorf("Could not clear old chunks from disk")
	}
	return nil
}

func (s *Storage) ExistsOrCreate(id string) bool {
	s.tocLock.Lock()
	if _, exists := s.toc[id]; exists {
		s.tocLock.Unlock()
		return true
	}
	s.toc[id] = &ChunkInfo{
		location: RAM,
		access:   time.Now(),
	}
	s.tocLock.Unlock()
	return false
}

func (s *Storage) Store(id string, bytes []byte) error {
	s.chunksLock.Lock()
	s.chunks[id] = bytes
	s.chunksLock.Unlock()

	s.queue <- &Item{
		id:    id,
		bytes: bytes,
	}

	return nil
}

func (s *Storage) Get(id string, offset, size int64) ([]byte, error) {
	res := make(chan []byte)

	go func() {
		for {
			s.tocLock.Lock()
			chunkInfo, exists := s.toc[id]
			s.tocLock.Unlock()
			if exists {
				if RAM == chunkInfo.location {
					bytes, exists := s.loadFromRAM(id, offset, size)
					if exists {
						res <- bytes
						return
					}
				} else if DISK == chunkInfo.location {
					bytes, exists := s.loadFromDisk(id, offset, size)
					if exists {
						res <- bytes
						return
					}
				}
			}

			time.Sleep(100 * time.Millisecond)
		}
	}()

	return <-res, nil
}

func (s *Storage) thread() {
	for {
		item := <-s.queue
		if err := s.storeToDisk(item.id, item.bytes); nil != err {
			Log.Warningf("%v", err)
		}
	}
}

func (s *Storage) loadFromRAM(id string, offset, size int64) ([]byte, bool) {
	bytes, exists := s.chunks[id]
	if !exists {
		return nil, false
	}

	sOffset := int64(math.Min(float64(len(bytes)), float64(offset)))
	eOffset := int64(math.Min(float64(len(bytes)), float64(offset+size)))
	return bytes[sOffset:eOffset], true
}

func (s *Storage) loadFromDisk(id string, offset, size int64) ([]byte, bool) {
	filename := filepath.Join(s.ChunkPath, id)

	f, err := os.Open(filename)
	if nil != err {
		Log.Tracef("%v", err)
		return nil, false
	}
	defer f.Close()

	buf := make([]byte, size)
	n, err := f.ReadAt(buf, offset)
	if n > 0 && (nil == err || io.EOF == err || io.ErrUnexpectedEOF == err) {
		eOffset := int64(math.Min(float64(size), float64(len(buf))))
		return buf[:eOffset], true
	}

	Log.Tracef("%v", err)
	return nil, false
}

func (s *Storage) storeToDisk(id string, bytes []byte) error {
	if s.getTempSize()+s.ChunkSize*2 > s.MaxTempSize {
		s.deleteOldestChunk()
	}

	filename := filepath.Join(s.ChunkPath, id)

	if _, err := os.Stat(s.ChunkPath); os.IsNotExist(err) {
		if err := os.MkdirAll(s.ChunkPath, 0777); nil != err {
			Log.Debugf("%v", err)
			return fmt.Errorf("Could not create chunk temp path %v", s.ChunkPath)
		}
	}

	if _, err := os.Stat(filename); os.IsNotExist(err) {
		if err := ioutil.WriteFile(filename, bytes, 0777); nil != err {
			Log.Debugf("%v", err)
			return fmt.Errorf("Could not write chunk temp file %v", filename)
		}
	}

	s.tocLock.Lock()
	info := s.toc[id]
	if nil != info {
		info.location = DISK
	}
	s.tocLock.Unlock()

	s.chunksLock.Lock()
	delete(s.chunks, id)
	s.chunksLock.Unlock()

	return nil
}

func (s *Storage) deleteOldestChunk() {
	var oldestID string
	oldestTime := time.Now()
	for id, info := range s.toc {
		if info.location == DISK && info.access.Before(oldestTime) {
			oldestID = id
		}
	}

	filename := filepath.Join(s.ChunkPath, oldestID)

	Log.Debugf("Deleting chunk %v", filename)
	if err := os.Remove(filename); nil != err {
		Log.Debugf("%v", err)
		Log.Warningf("Could not delete chunk %v", filename)
	}

	s.tocLock.Lock()
	delete(s.toc, oldestID)
	s.tocLock.Unlock()
}

func (s *Storage) getTempSize() int64 {
	count := int64(0)
	s.tocLock.Lock()
	for _, info := range s.toc {
		if info.location == DISK {
			count++
		}
	}
	s.tocLock.Unlock()

	return count * s.ChunkSize
}