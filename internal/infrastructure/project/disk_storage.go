package project

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gisquick/gisquick-server/internal/domain"
	"github.com/jellydator/ttlcache/v3"
	"go.uber.org/zap"
)

type FilesIndex struct {
	sync.RWMutex
	Index map[string]FileInfo
}

func (fi *FilesIndex) Get(path string) (FileInfo, bool) {
	fi.RLock()
	defer fi.RUnlock()
	val, exists := fi.Index[path]
	return val, exists
}

func (fi *FilesIndex) Set(path string, info FileInfo) {
	fi.Lock()
	defer fi.Unlock()
	fi.Index[path] = info
}

func (fi *FilesIndex) Delete(path string) {
	fi.Lock()
	defer fi.Unlock()
	delete(fi.Index, path)
}

func (fi *FilesIndex) DeleteDir(dirPath string) {
	fi.Lock()
	defer fi.Unlock()

	dirPrefix := strings.TrimSuffix(dirPath, string(filepath.Separator)) + string(filepath.Separator)
	for p, _ := range fi.Index {
		if strings.HasPrefix(p, dirPrefix) {
			delete(fi.Index, p)
		}
	}
}

func (fi *FilesIndex) TotalSize() int64 {
	fi.RLock()
	defer fi.RUnlock()
	size := int64(0)
	for _, info := range fi.Index {
		size += info.Size
	}
	return size
}

type DiskStorage struct {
	ProjectsRoot   string
	MaxProjectSize int64
	log            *zap.SugaredLogger
	indexCache     *ttlcache.Cache[string, *FilesIndex]
}

type Info struct {
	Title       string `json:"title"`
	File        string `json:"file"`
	ProjectHash string `json:"project_hash"`
	Projection  string `json:"projection"`
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}

// func (s *DiskStorage) GetProject(name string) (*domain.Project, error) {
// 	loadFn := func(key string) (interface{}, time.Duration, error) {
// 		proj, err := s.loadProjectData(key)
// 		return proj, 0, err
// 	}
// 	p, err := s.cache.GetByLoader(project, loadFn)
// 	return p.(*domain.Project), err
// }

func DBHash(path string) (string, error) {
	cmdOut, err := exec.Command("dbhash", path).Output()
	if err != nil {
		return "", fmt.Errorf("executing dbhash command: %w", err)
	}
	hash := strings.Split(string(cmdOut), " ")[0]
	return hash, nil
}

// Checksum computes SHA-1 hash of file
func Sha1(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha1.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func Checksum(path string) (string, error) {
	if strings.ToLower(filepath.Ext(path)) == ".gpkg" {
		dbhash, err := DBHash(path)
		return "dbhash:" + dbhash, err
	}
	return Sha1(path)
}

var excludeExtRegex = regexp.MustCompile(`(?i).*\.(gpkg-wal|gpkg-shm)$`)

func NewDiskStorage(log *zap.SugaredLogger, projectsRoot string) *DiskStorage {
	ds := &DiskStorage{
		ProjectsRoot: projectsRoot,
		log:          log,
	}
	loader := ttlcache.LoaderFunc[string, *FilesIndex](
		func(c *ttlcache.Cache[string, *FilesIndex], project string) *ttlcache.Item[string, *FilesIndex] {
			log.Infof("ttlcache.LoaderFunc: %s", project)

			indexData, err := ds.loadFilesIndex(project)
			if err != nil {
				log.Errorw("failed to read files index file", "project", project, zap.Error(err))
				files, err := ds.createFilesMap(project)
				if err != nil {
					log.Errorw("failed to list project files", "project", project, zap.Error(err))
					// TODO: return nil or empty index?
					// var emptyIndex map[string]*FileInfo
					// emptyIndex := &FilesIndex{Index: emptyIndex}
					return nil
				}
				for path, info := range files {
					absPath := filepath.Join(projectsRoot, project, path)
					hash, err := Checksum(absPath)
					if err != nil {
						log.Errorw("failed to list project files", "project", project, zap.Error(err))
						return nil
					}
					// info := files[path]
					info.Hash = hash
					files[path] = info
				}
				log.Info("new index files", files)
				indexData = files
			}
			index := &FilesIndex{Index: indexData}
			item := c.Set(project, index, ttlcache.DefaultTTL)
			return item
		},
	)
	indexCache := ttlcache.New(
		ttlcache.WithTTL[string, *FilesIndex](12*time.Hour),
		// ttlcache.WithTTL[string, *FilesIndex](1*time.Minute),
		ttlcache.WithLoader[string, *FilesIndex](loader),
		ttlcache.WithDisableTouchOnHit[string, *FilesIndex](),
	)
	ds.indexCache = indexCache
	indexCache.OnEviction(func(ctx context.Context, er ttlcache.EvictionReason, i *ttlcache.Item[string, *FilesIndex]) {
		project := i.Key()
		index := i.Value()
		log.Infow("ttlcache.OnEviction.indexCache", "project", project)
		if err := saveJsonFile(filepath.Join(projectsRoot, project, ".gisquick", "filesmap.json"), index.Index); err != nil {
			log.Errorw("saving files index", "project", project, zap.Error(err))
		}
	})
	go indexCache.Start()
	return ds
}

func saveJsonFile(path string, data interface{}) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	if err := encoder.Encode(data); err != nil {
		return err
	}
	return nil
}

func (s *DiskStorage) saveConfigFile(projectName, filename string, data interface{}) error {
	indexFilePath := filepath.Join(s.ProjectsRoot, projectName, ".gisquick", filename)
	if err := saveJsonFile(indexFilePath, data); err != nil {
		return fmt.Errorf("creating project file: %w", err)
	}
	return nil
}

func (s *DiskStorage) Create(fullName string, meta json.RawMessage) (*domain.ProjectInfo, error) {
	projDir := filepath.Join(s.ProjectsRoot, fullName)
	internalDir := filepath.Join(projDir, ".gisquick")
	if s.CheckProjectExists(fullName) {
		return nil, domain.ErrProjectAlreadyExists
	}
	if err := os.MkdirAll(internalDir, 0777); err != nil {
		return nil, err
	}

	var i Info
	if err := json.Unmarshal(meta, &i); err != nil {
		s.log.Errorw("parsing qgis meta", zap.Error(err))
		return nil, domain.ErrInvalidQgisMeta
	}

	if err := s.saveConfigFile(fullName, "qgis.json", meta); err != nil {
		return nil, fmt.Errorf("creating qgis meta file: %w", err)
	}

	info := domain.ProjectInfo{
		QgisFile:   i.File,
		Projection: i.Projection,
		Title:      i.Title,
		State:      "empty",
		Created:    time.Now().UTC(),
	}
	return &info, s.saveConfigFile(fullName, "project.json", info)
}

func (s *DiskStorage) UserProjects(username string) ([]string, error) {
	projectsNames := make([]string, 0)
	userDir := filepath.Join(s.ProjectsRoot, username)
	entries, err := os.ReadDir(userDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return projectsNames, nil
		}
		return projectsNames, fmt.Errorf("listing projects: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			projectName := filepath.Join(username, entry.Name())
			projPath := filepath.Join(userDir, entry.Name(), ".gisquick", "project.json")
			if fileExists(projPath) {
				projectsNames = append(projectsNames, projectName)
			}
		}
	}
	return projectsNames, nil
}

func (s *DiskStorage) CheckProjectExists(name string) bool {
	projPath := filepath.Join(s.ProjectsRoot, name, ".gisquick", "project.json")
	return fileExists(projPath)
}

func (s *DiskStorage) GetProjectInfo(name string) (domain.ProjectInfo, error) {
	var pInfo domain.ProjectInfo
	projPath := filepath.Join(s.ProjectsRoot, name, ".gisquick", "project.json")

	// ver. 1
	/*
		f, err := os.Open(projPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return pInfo, domain.ErrProjectNotExists
			}
		}
		defer f.Close()
		decoder := json.NewDecoder(f)
		if err := decoder.Decode(&pInfo); err != nil {
			s.log.Errorw("parsing project file", zap.Error(err))
			return pInfo, fmt.Errorf("reading project file: %w", err)
		}
	*/
	// ver. 2
	content, err := ioutil.ReadFile(projPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pInfo, domain.ErrProjectNotExists
		}
		return pInfo, fmt.Errorf("reading project file: %w", err)
	}
	err = json.Unmarshal(content, &pInfo)
	if err != nil {
		s.log.Errorw("parsing project file", zap.Error(err))
		return pInfo, fmt.Errorf("reading project file: %w", err)
	}
	return pInfo, nil
}

// func (s *DiskStorage) saveFileIndex(project string, index *FilesIndex) {
// 	if err := saveJsonFile(filepath.Join(s.ProjectsRoot, project, ".gisquick", "filesmap.json"), index); err != nil {
// 		return nil, fmt.Errorf("saving files index: %w", err)
// 	}
// }

func (s *DiskStorage) createFilesMap(project string) (map[string]FileInfo, error) {
	files := make(map[string]FileInfo)
	root, err := filepath.Abs(filepath.Join(s.ProjectsRoot, project))
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			relPath := path[len(root)+1:]
			if !strings.HasPrefix(relPath, ".gisquick/") && !strings.HasSuffix(relPath, "~") && !excludeExtRegex.Match([]byte(relPath)) {
				fInfo, err := entry.Info()
				if err != nil {
					return fmt.Errorf("getting file info: %w", err)
				}
				files[relPath] = FileInfo{Size: fInfo.Size(), Mtime: fInfo.ModTime().Unix()}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing project files: %w", err)
	}
	return files, nil
}

// TODO: update files index when not up to date
/*
func (s *DiskStorage) ListProjectFiles1(project string, checksum bool) ([]domain.ProjectFile, error) {
	if !s.CheckProjectExists(project) {
		return nil, domain.ErrProjectNotExists
	}
	index, err := s.filesIndex(project)
	if err != nil {
		// log error and continue without index
		s.log.Errorw("getting files index", zap.Error(err))
	}

	root, err := filepath.Abs(filepath.Join(s.ProjectsRoot, project))
	if err != nil {
		return nil, err
	}
	var files []domain.ProjectFile = []domain.ProjectFile{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			relPath := path[len(root)+1:]
			if !strings.HasPrefix(relPath, ".gisquick/") && !strings.HasSuffix(relPath, "~") && !excludeExtRegex.Match([]byte(relPath)) {
				fInfo, err := entry.Info()
				if err != nil {
					return fmt.Errorf("getting file info: %w", err)
				}
				finfo := domain.ProjectFile{Path: relPath, Size: fInfo.Size(), Mtime: fInfo.ModTime()}
				if checksum {
					cachedInfo, hasCachedInfo := index.Get(relPath)
					if hasCachedInfo && cachedInfo.Mtime == fInfo.ModTime().Unix() {
						finfo.Hash = cachedInfo.Hash
					} else {
						hash, err := Checksum(path)
						if err != nil {
							return fmt.Errorf("computing checksum: %w", err)
						}
						finfo.Hash = hash
						index.Set(relPath, FileInfo{Hash: finfo.Hash, Size: finfo.Size, Mtime: finfo.Mtime.Unix()})
					}
				}
				files = append(files, finfo)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing project files: %w", err)
	}
	return files, nil
}
*/

func (s *DiskStorage) ListProjectFiles(project string, checksum bool) ([]domain.ProjectFile, error) {
	if !s.CheckProjectExists(project) {
		return nil, domain.ErrProjectNotExists
	}
	filesMap, err := s.createFilesMap(project)
	if err != nil {
		return nil, fmt.Errorf("listing project files: %w", err)
	}
	index, err := s.filesIndex(project)
	if err != nil {
		s.log.Errorw("reading files index", "project", project, zap.Error(err))
		// return nil, fmt.Errorf("loading files index: %w", err)
	}
	indexUpdated := false
	files := make([]domain.ProjectFile, len(filesMap))
	i := 0
	for path, info := range filesMap {
		f := domain.ProjectFile{
			Path:  path,
			Size:  info.Size,
			Mtime: time.Unix(info.Mtime, 0),
		}
		if checksum {
			cachedInfo, hasCachedInfo := index.Get(path)
			if hasCachedInfo && cachedInfo.Mtime == info.Mtime {
				f.Hash = cachedInfo.Hash
			} else {
				absPath := filepath.Join(s.ProjectsRoot, project, path)
				hash, err := Checksum(absPath)
				if err != nil {
					return nil, fmt.Errorf("computing checksum: %w", err)
				}
				f.Hash = hash
				// update file info in the index
				index.Set(path, FileInfo{Hash: hash, Size: info.Size, Mtime: info.Mtime})
				indexUpdated = true
				s.log.Debugw("updating files index", "path", path)
			}
		}
		files[i] = f
		i += 1
	}
	// index.RLock()
	// defer index.RUnlock()
	for path := range index.Index {
		if _, exists := filesMap[path]; !exists {
			index.Delete(path)
			indexUpdated = true
			s.log.Debugw("cleaning files index", "path", path)
		}
	}
	if indexUpdated {
		projectInfo, err := s.GetProjectInfo(project)
		if err != nil {
			s.log.Errorw("updating project size", "project", project, zap.Error(err))
		}
		projectInfo.Size = index.TotalSize()
		if err := s.saveConfigFile(project, "project.json", projectInfo); err != nil {
			s.log.Errorw("updating project size", "project", project, zap.Error(err))
		}
	}
	return files, nil
}

func (s *DiskStorage) Delete(name string) error {
	dest := filepath.Join(s.ProjectsRoot, name)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	// delete Mapcache
	return nil
}

func saveToFile(src io.Reader, filename string) (err error) {
	err = os.MkdirAll(filepath.Dir(filename), 0777)
	if err != nil {
		return err
	}
	file, err := os.Create(filename)
	if err != nil {
		return err
	}

	// more verbose but with better errors propagation
	defer func() {
		if cerr := file.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if _, err := io.Copy(file, src); err != nil {
		return err
	}
	return nil
}

func saveToFile2(src io.Reader, filename string) (h string, err error) {
	err = os.MkdirAll(filepath.Dir(filename), 0777)
	if err != nil {
		return
	}
	file, err := os.Create(filename)
	if err != nil {
		return
	}
	defer func() {
		// Clean up in case we are returning with an error
		if err != nil {
			file.Close()
			os.Remove(file.Name())
		}
	}()

	sha := sha1.New()
	dest := io.MultiWriter(file, sha)

	if _, err := io.Copy(dest, src); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return
	}
	hash := fmt.Sprintf("%x", sha.Sum(nil))
	return hash, nil
}

// Currently not used
func (s *DiskStorage) CreateFile(projectName, pattern string, r io.Reader, size int64) (finfo domain.ProjectFile, err error) {
	finfo = domain.ProjectFile{}
	if !s.CheckProjectExists(projectName) {
		err = domain.ErrProjectNotExists
		return
	}
	index, err := s.filesIndex(projectName)
	if err != nil {
		err = fmt.Errorf("reading files index: %w", err)
		return
	}
	projectSize := index.TotalSize()
	if projectSize+size > s.MaxProjectSize {
		err = domain.ErrProjectSize
		return
	}
	destDir := filepath.Join(s.ProjectsRoot, projectName, ".temp")
	err = os.MkdirAll(destDir, 0777)
	if err != nil {
		err = fmt.Errorf("creating .temp directory: %w", err)
		return
	}
	f, err := os.CreateTemp(destDir, pattern)
	if err != nil {
		err = fmt.Errorf("creating temp file: %w", err)
		return
	}
	s.log.Infow("created temporary file", "path", f.Name())
	defer func() {
		// Clean up in case we are returning with an error
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if err = f.Chmod(0644); err != nil {
		return
	}
	sha := sha1.New()
	dest := io.MultiWriter(f, sha)
	if _, err = io.Copy(dest, r); err != nil {
		return
	}
	if err = f.Close(); err != nil {
		return
	}
	fStat, err := os.Stat(f.Name())
	if err != nil {
		return
	}
	finfo.Size = fStat.Size()
	finfo.Mtime = fStat.ModTime()
	finfo.Path = f.Name()
	finfo.Hash = fmt.Sprintf("%x", sha.Sum(nil))
	s.log.Infow("SaveFile", "path", f.Name(), "hash", finfo.Hash)
	return
}

func (s *DiskStorage) SaveFile(project string, finfo domain.ProjectFile, path string) error {
	absPath := filepath.Join(s.ProjectsRoot, project, path)
	if err := os.MkdirAll(filepath.Dir(absPath), 0777); err != nil {
		return err
	}
	if err := os.Rename(finfo.Path, absPath); err != nil {
		return fmt.Errorf("saving project file: %w", err)
	}
	index, err := s.filesIndex(project)
	if err != nil {
		s.log.Errorw("reading files index", "project", project, zap.Error(err))
		return nil
	}
	if fStat, err := os.Stat(absPath); err == nil {
		s.log.Infow("mtime check", "orig", finfo.Mtime.Unix(), "after rename", fStat.ModTime().Unix())
	}
	index.Set(path, FileInfo{Hash: finfo.Hash, Size: finfo.Size, Mtime: finfo.Mtime.Unix()})
	// TODO: update project size
	return nil
}

func (s *DiskStorage) GetQgisMetaPath(projectName string) string {
	return filepath.Join(s.ProjectsRoot, projectName, ".gisquick", "qgis.json")
}

func (s *DiskStorage) GetSettingsPath(projectName string) string {
	return filepath.Join(s.ProjectsRoot, projectName, ".gisquick", "settings.json")
}

func (s *DiskStorage) GetThumbnailPath(projectName string) string {
	return filepath.Join(s.ProjectsRoot, projectName, ".gisquick", "thumbnail")
}

func (s *DiskStorage) SaveThumbnail(projectName string, r io.Reader) error {
	project, err := s.GetProjectInfo(projectName)
	if err != nil {
		return err
	}
	if err := saveToFile(r, s.GetThumbnailPath(projectName)); err != nil {
		return fmt.Errorf("saving thumbnail file: %w", err)
	}
	project.Thumbnail = true
	project.LastUpdate = time.Now().UTC()
	if err := s.saveConfigFile(projectName, "project.json", project); err != nil {
		return fmt.Errorf("updating project file: %w", err)
	}
	return nil
}

// func (s *DiskStorage) filesIndex1(projectName string) ([]domain.ProjectFile, error) {
// 	var files []domain.ProjectFile
// 	if !s.CheckProjectExists(projectName) {
// 		return files, domain.ErrProjectNotExists
// 	}
// 	indexPath := filepath.Join(s.ProjectsRoot, projectName, ".gisquick", "files.json")
// 	f, err := os.Open(indexPath)
// 	if err != nil {
// 		if errors.Is(err, os.ErrNotExist) {
// 			return files, nil
// 		}
// 		return files, fmt.Errorf("reading index file: %w", err)
// 	}
// 	defer f.Close()
// 	decoder := json.NewDecoder(f)
// 	if err := decoder.Decode(&files); err != nil {
// 		// s.log.Errorw("parsing project files index", zap.Error(err))
// 		return files, fmt.Errorf("parsing index file: %w", err)
// 	}
// 	return files, nil
// }

func (s *DiskStorage) loadFilesIndex(projectName string) (map[string]FileInfo, error) {
	s.log.Infow("loading filesIndex", "project", projectName)
	var index map[string]FileInfo
	indexPath := filepath.Join(s.ProjectsRoot, projectName, ".gisquick", "filesmap.json")
	f, err := os.Open(indexPath)
	if err != nil {
		index = make(map[string]FileInfo)
		if errors.Is(err, os.ErrNotExist) {
			return index, nil
		}
		return index, fmt.Errorf("reading index file: %w", err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&index); err != nil {
		// s.log.Errorw("parsing project files index", zap.Error(err))
		return make(map[string]FileInfo), fmt.Errorf("parsing index file: %w", err)
	}
	return index, nil
}

func (s *DiskStorage) filesIndex(projectName string) (*FilesIndex, error) {
	var index *FilesIndex
	if !s.CheckProjectExists(projectName) {
		return index, domain.ErrProjectNotExists
		// return make(map[string]FileInfo), domain.ErrProjectNotExists
	}
	fi := s.indexCache.Get(projectName)
	if fi == nil {
		return index, fmt.Errorf("loading project files index: %s", projectName)
	}
	return fi.Value(), nil
}

type FileInfo struct {
	Hash  string `json:"hash"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	// Mtime time.Time `json:"mtime"`
}

// func createFilesIndex(files []domain.ProjectFile) map[string]FileInfo {
// 	index := make(map[string]FileInfo, len(files))
// 	for _, f := range files {
// 		index[f.Path] = FileInfo{Hash: f.Hash, Size: f.Size, Mtime: time.Now().Unix()}
// 	}
// 	return index
// }

func calcNewSize(index *FilesIndex, info domain.FilesChanges) int64 {
	index.RLock()
	// defer index.RUnlock()
	sizeMap := make(map[string]int64, len(index.Index))
	for path, f := range index.Index {
		sizeMap[path] = f.Size
	}
	index.RUnlock()

	for _, path := range info.Removes {
		// TODO: support for deleting of directories?
		delete(sizeMap, path)
	}
	for _, f := range info.Updates {
		sizeMap[f.Path] = f.Size
	}
	var sum int64 = 0
	for _, size := range sizeMap {
		sum += size
	}
	return sum
}

func indexProjectFilesList(index *FilesIndex) []domain.ProjectFile {
	index.RLock()
	defer index.RUnlock()
	listIndex := make([]domain.ProjectFile, len(index.Index))
	i := 0
	for path, info := range index.Index {
		listIndex[i] = domain.ProjectFile{Path: path, Hash: info.Hash, Size: info.Size, Mtime: time.Unix(info.Mtime, 0)}
		i += 1
	}
	return listIndex
}

func (s *DiskStorage) UpdateFiles(projectName string, info domain.FilesChanges, next func() (string, io.ReadCloser, error)) ([]domain.ProjectFile, error) { // ([]domain.ProjectFile, error)
	project, err := s.GetProjectInfo(projectName)
	if err != nil {
		return nil, err
	}
	index, err := s.filesIndex(projectName)
	if err != nil {
		return nil, err
	}
	expectedSize := calcNewSize(index, info)
	if len(info.Updates) > 0 && s.MaxProjectSize > 0 && expectedSize > s.MaxProjectSize {
		return nil, domain.ErrProjectSize
	}
	files := info.Updates

	// i := 0
	// for {
	// 	path, reader, err := next()
	// 	if err != nil {
	// 		if err == io.EOF {
	// 			break
	// 		}
	// 		return nil, err
	// 	}
	// 	if i >= len(files) {
	// 		return nil, fmt.Errorf("missing file change metadata: %s", path)
	// 	}

	if len(files) > 0 && next == nil {
		return nil, fmt.Errorf("required function for reading uploaded files")
	}
	for i := 0; i < len(files); i++ {
		path, reader, err := next()
		if err != nil {
			return nil, fmt.Errorf("reading upload files stream: %w", err)
		}
		declaredInfo := files[i]
		if declaredInfo.Path != path {
			return nil, err // TODO: more graceful error handling
		}
		absPath := filepath.Join(s.ProjectsRoot, projectName, path)
		// if err := saveToFile(reader, absPath); err != nil {
		// 	return err
		// }
		calcHash, err := saveToFile2(reader, absPath)
		if err != nil {
			reader.Close() // or move to saveToFile?
			return nil, err
		}
		// s.log.Infow("saving file", "path", absPath, "hash", calcHash, "hashMatch", declaredInfo.Hash == calcHash)
		reader.Close()
		fStat, err := os.Stat(absPath)
		if err != nil {
			// ???
			return nil, err
		}
		if declaredInfo.Size != fStat.Size() {
			return nil, fmt.Errorf("declared file info doesn't match: %s", path)
		}
		index.Set(path, FileInfo{Hash: calcHash, Size: declaredInfo.Size, Mtime: fStat.ModTime().Unix()})
		// i += 1
	}
	for _, path := range info.Removes {
		absPath := filepath.Join(s.ProjectsRoot, projectName, path)
		info, err := os.Lstat(absPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				index.Delete(path)
				continue
			}
			return nil, fmt.Errorf("removing file/directory %s: %w", path, err)
		}
		if info.IsDir() {
			if err := os.RemoveAll(absPath); err != nil {
				return nil, fmt.Errorf("removing project directory %s: %w", path, err) // TODO: or allow this kind of error?
			}
			index.DeleteDir(path)
		} else {
			if err := os.Remove(absPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("removing project file %s: %w", path, err) // TODO: or allow this kind of error?
			}
			index.Delete(path)
		}
	}
	if err := saveJsonFile(filepath.Join(s.ProjectsRoot, projectName, ".gisquick", "filesmap.json"), index); err != nil {
		return nil, fmt.Errorf("saving files index: %w", err)
	}
	size := index.TotalSize()
	s.log.Infof("project size: %d / expected: %d", size, expectedSize)
	project.Size = size
	if project.State == "empty" {
		project.State = "staged"
	}
	project.LastUpdate = time.Now().UTC()
	if err := s.saveConfigFile(projectName, "project.json", project); err != nil {
		return nil, fmt.Errorf("updating project file: %w", err)
	}
	return indexProjectFilesList(index), nil
}

func (s *DiskStorage) RemoveFiles(projectName string, files ...string) ([]domain.ProjectFile, error) { // ([]domain.ProjectFile, error)
	project, err := s.GetProjectInfo(projectName)
	if err != nil {
		return nil, err
	}
	index, err := s.filesIndex(projectName)
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		absPath := filepath.Join(s.ProjectsRoot, projectName, path)
		info, err := os.Lstat(absPath)
		if err != nil {
			return nil, fmt.Errorf("removing file/directory %s: %w", path, err)
		}
		if info.IsDir() {
			if err := os.RemoveAll(absPath); err != nil {
				return nil, fmt.Errorf("removing project directory %s: %w", path, err) // TODO: or allow this kind of error?
			}
			index.DeleteDir(path)
		} else {
			if err := os.Remove(absPath); err != nil {
				return nil, fmt.Errorf("removing project file %s: %w", path, err) // TODO: or allow this kind of error?
			}
			index.Delete(path)
		}
	}
	if err := saveJsonFile(filepath.Join(s.ProjectsRoot, projectName, ".gisquick", "filesmap.json"), index); err != nil {
		return nil, fmt.Errorf("saving files index: %w", err)
	}
	project.Size = index.TotalSize()
	project.LastUpdate = time.Now().UTC()
	if err := s.saveConfigFile(projectName, "project.json", project); err != nil {
		return nil, fmt.Errorf("updating project file: %w", err)
	}
	return indexProjectFilesList(index), nil
}

type SettingsInfo struct {
	Title string `json:"title"`
	Auth  struct {
		Type string `json:"type"`
	} `json:"auth"`
}

func (s *DiskStorage) UpdateSettings(projectName string, data json.RawMessage) error {
	project, err := s.GetProjectInfo(projectName)
	if err != nil {
		return err
	}
	var sInfo SettingsInfo
	if err := json.Unmarshal(data, &sInfo); err != nil {
		return fmt.Errorf("extracting authentication settings: %w", err)
	}
	if err := s.saveConfigFile(projectName, "settings.json", data); err != nil {
		return fmt.Errorf("saving settings file: %w", err)
	}
	project.State = "published"
	project.LastUpdate = time.Now().UTC()
	project.Authentication = sInfo.Auth.Type
	project.Title = sInfo.Title
	if err := s.saveConfigFile(projectName, "project.json", project); err != nil {
		return fmt.Errorf("updating project file: %w", err)
	}
	return nil
}

func (s *DiskStorage) GetSettings(projectName string) (domain.ProjectSettings, error) {
	var settings domain.ProjectSettings
	content, err := os.ReadFile(s.GetSettingsPath(projectName))
	if err != nil {
		return settings, err
	}
	err = json.Unmarshal(content, &settings)
	// err = jsoniter.Unmarshal(content, &settings)
	return settings, err
}

func (s *DiskStorage) ParseQgisMetadata(projectName string, data interface{}) error {
	content, err := os.ReadFile(s.GetQgisMetaPath(projectName))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, &data); err != nil {
		return err
	}
	return nil
}

func (s *DiskStorage) UpdateMeta(projectName string, meta json.RawMessage) error {
	pInfo, err := s.GetProjectInfo(projectName)
	if err != nil {
		return err
	}
	var i Info
	if err := json.Unmarshal(meta, &i); err != nil {
		s.log.Errorw("parsing qgis meta", zap.Error(err))
		return domain.ErrInvalidQgisMeta
	}

	if err := s.saveConfigFile(projectName, "qgis.json", meta); err != nil {
		return fmt.Errorf("creating qgis meta file: %w", err)
	}

	pInfo.QgisFile = i.File
	pInfo.Projection = i.Projection
	pInfo.Title = i.Title
	pInfo.LastUpdate = time.Now().UTC()
	return s.saveConfigFile(projectName, "project.json", pInfo)
}

func (s *DiskStorage) GetScripts(projectName string) (domain.Scripts, error) {
	file := filepath.Join(s.ProjectsRoot, projectName, ".gisquick", "scripts.json")
	content, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var data domain.Scripts
	err = json.Unmarshal(content, &data)
	return data, nil
}

func (s *DiskStorage) UpdateScripts(projectName string, scripts domain.Scripts) error {
	return s.saveConfigFile(projectName, "scripts.json", scripts)
}

func (s *DiskStorage) Close() {
	s.indexCache.Stop()
	s.indexCache.DeleteAll()
}
