package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"database/sql"
	"math/rand"
	"sort"
	"syscall"

	"github.com/haiwen/seafile-server/fileserver/blockmgr"
	"github.com/haiwen/seafile-server/fileserver/commitmgr"
	"github.com/haiwen/seafile-server/fileserver/fsmgr"
	"github.com/haiwen/seafile-server/fileserver/repomgr"
)

//contentType = "application/octet-stream"
func parseContentType(fileName string) string {
	var contentType string

	parts := strings.Split(fileName, ".")
	if len(parts) >= 2 {
		suffix := parts[len(parts)-1]
		switch suffix {
		case "txt":
			contentType = "text/plain"
		case "doc":
			contentType = "application/vnd.ms-word"
		case "docx":
			contentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
		case "ppt":
			contentType = "application/vnd.ms-powerpoint"
		case "xls":
			contentType = "application/vnd.ms-excel"
		case "xlsx":
			contentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		case "pdf":
			contentType = "application/pdf"
		case "zip":
			contentType = "application/zip"
		case "mp3":
			contentType = "audio/mp3"
		case "mpeg":
			contentType = "video/mpeg"
		case "mp4":
			contentType = "video/mp4"
		case "jpeg", "JPEG", "jpg", "JPG":
			contentType = "image/jpeg"
		case "png", "PNG":
			contentType = "image/png"
		case "gif", "GIF":
			contentType = "image/gif"
		case "svg", "SVG":
			contentType = "image/svg+xml"
		}
	}

	return contentType
}

func testFireFox(r *http.Request) bool {
	userAgent, ok := r.Header["User-Agent"]
	if !ok {
		return false
	}

	userAgentStr := strings.Join(userAgent, "")
	if strings.Index(userAgentStr, "firefox") != -1 {
		return true
	}

	return false
}

func accessCB(rsp http.ResponseWriter, r *http.Request) *appError {
	parts := strings.Split(r.URL.Path[1:], "/")
	if len(parts) < 3 {
		msg := "Invalid URL"
		return &appError{nil, msg, http.StatusBadRequest}
	}
	token := parts[1]
	fileName := parts[2]
	accessInfo, err := parseWebaccessInfo(rsp, token)
	if err != nil {
		return err
	}

	repoID := accessInfo.repoID
	op := accessInfo.op
	user := accessInfo.user
	objID := accessInfo.objID

	if op != "view" && op != "download" && op != "download-link" {
		msg := "Bad access token"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	if _, ok := r.Header["If-Modified-Since"]; ok {
		return &appError{nil, "", http.StatusNotModified}
	}

	now := time.Now()
	rsp.Header().Set("Last-Modified", now.Format("Mon, 2 Jan 2006 15:04:05 GMT"))
	rsp.Header().Set("Cache-Control", "max-age=3600")

	ranges := r.Header["Range"]
	byteRanges := strings.Join(ranges, "")

	repo := repomgr.Get(repoID)
	if repo == nil {
		msg := "Bad repo id"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	var cryptKey *seafileCrypt
	if repo.IsEncrypted {
		key, err := parseCryptKey(rsp, repoID, user)
		if err != nil {
			return err
		}
		cryptKey = key
	}

	exists, _ := fsmgr.Exists(repo.StoreID, objID)
	if !exists {
		msg := "Invalid file id"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	if !repo.IsEncrypted && len(byteRanges) != 0 {
		if err := doFileRange(rsp, r, repo, objID, fileName, op, byteRanges, user); err != nil {
			return err
		}
	} else if err := doFile(rsp, r, repo, objID, fileName, op, cryptKey, user); err != nil {
		return err
	}

	return nil
}

type seafileCrypt struct {
	key []byte
	iv  []byte
}

func parseCryptKey(rsp http.ResponseWriter, repoID string, user string) (*seafileCrypt, *appError) {
	key, err := rpcclient.Call("seafile_get_decrypt_key", repoID, user)
	if err != nil {
		errMessage := "Repo is encrypted. Please provide password to view it."
		return nil, &appError{nil, errMessage, http.StatusBadRequest}
	}

	cryptKey, ok := key.(map[string]interface{})
	if !ok {
		err := fmt.Errorf("failed to assert crypt key.\n")
		return nil, &appError{err, "", http.StatusInternalServerError}
	}

	seafileKey := new(seafileCrypt)

	if cryptKey != nil {
		key, ok := cryptKey["key"].(string)
		if !ok {
			err := fmt.Errorf("failed to parse crypt key.\n")
			return nil, &appError{err, "", http.StatusInternalServerError}
		}
		iv, ok := cryptKey["iv"].(string)
		if !ok {
			err := fmt.Errorf("failed to parse crypt iv.\n")
			return nil, &appError{err, "", http.StatusInternalServerError}
		}
		seafileKey.key, err = hex.DecodeString(key)
		if err != nil {
			err := fmt.Errorf("failed to decode key: %v.\n", err)
			return nil, &appError{err, "", http.StatusInternalServerError}
		}
		seafileKey.iv, err = hex.DecodeString(iv)
		if err != nil {
			err := fmt.Errorf("failed to decode iv: %v.\n", err)
			return nil, &appError{err, "", http.StatusInternalServerError}
		}
	}

	return seafileKey, nil
}

func doFile(rsp http.ResponseWriter, r *http.Request, repo *repomgr.Repo, fileID string,
	fileName string, operation string, cryptKey *seafileCrypt, user string) *appError {
	file, err := fsmgr.GetSeafile(repo.StoreID, fileID)
	if err != nil {
		msg := "Failed to get seafile"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	var encKey, encIv []byte
	if cryptKey != nil {
		encKey = cryptKey.key
		encIv = cryptKey.iv
	}

	rsp.Header().Set("Access-Control-Allow-Origin", "*")

	setCommonHeaders(rsp, r, operation, fileName)

	//filesize string
	fileSize := fmt.Sprintf("%d", file.FileSize)
	rsp.Header().Set("Content-Length", fileSize)

	if r.Method == "HEAD" {
		rsp.WriteHeader(http.StatusOK)
		return nil
	}
	if file.FileSize == 0 {
		rsp.WriteHeader(http.StatusOK)
		return nil
	}

	if cryptKey != nil {
		for _, blkID := range file.BlkIDs {
			var buf bytes.Buffer
			blockmgr.Read(repo.StoreID, blkID, &buf)
			decoded, err := decrypt(buf.Bytes(), encKey, encIv)
			if err != nil {
				err := fmt.Errorf("failed to decrypt block %s: %v.\n", blkID, err)
				return &appError{err, "", http.StatusInternalServerError}
			}
			_, err = rsp.Write(decoded)
			if err != nil {
				log.Printf("failed to write block %s to response: %v.\n", blkID, err)
				return nil
			}
		}
		return nil
	}

	for _, blkID := range file.BlkIDs {
		err := blockmgr.Read(repo.StoreID, blkID, rsp)
		if err != nil {
			log.Printf("fatild to write block %s to response: %v.\n", blkID, err)
			return nil
		}
	}

	return nil
}

func doFileRange(rsp http.ResponseWriter, r *http.Request, repo *repomgr.Repo, fileID string,
	fileName string, operation string, byteRanges string, user string) *appError {

	file, err := fsmgr.GetSeafile(repo.StoreID, fileID)
	if err != nil {
		msg := "Failed to get seafile"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	if file.FileSize == 0 {
		rsp.WriteHeader(http.StatusOK)
		return nil
	}

	start, end, ok := parseRange(byteRanges, file.FileSize)
	if !ok {
		conRange := fmt.Sprintf("bytes */%d", file.FileSize)
		rsp.Header().Set("Content-Range", conRange)
		return &appError{nil, "", http.StatusRequestedRangeNotSatisfiable}
	}

	rsp.Header().Set("Accept-Ranges", "bytes")

	setCommonHeaders(rsp, r, operation, fileName)

	//filesize string
	conLen := fmt.Sprintf("%d", end-start+1)
	rsp.Header().Set("Content-Length", conLen)

	conRange := fmt.Sprintf("bytes %d-%d/%d", start, end, file.FileSize)
	rsp.Header().Set("Content-Range", conRange)

	var blkSize []uint64
	for _, v := range file.BlkIDs {
		size, err := blockmgr.Stat(repo.StoreID, v)
		if err != nil {
			err := fmt.Errorf("failed to stat block %s : %v.\n", v, err)
			return &appError{err, "", http.StatusInternalServerError}
		}
		blkSize = append(blkSize, uint64(size))
	}

	var off uint64
	var pos uint64
	var startBlock int
	for i, v := range blkSize {
		pos = start - off
		off += v
		if off > start {
			startBlock = i
			break
		}
	}

	// Read block from the start block and specified position
	var i int
	for ; i < len(file.BlkIDs); i++ {
		if i < startBlock {
			continue
		}

		blkID := file.BlkIDs[i]
		var buf bytes.Buffer
		if end-start+1 <= blkSize[i]-pos {
			err := blockmgr.Read(repo.StoreID, blkID, &buf)
			if err != nil {
				log.Printf("failed to read block %s: %v.\n", blkID, err)
				return nil
			}
			recvBuf := buf.Bytes()
			_, err = rsp.Write(recvBuf[pos : pos+end-start+1])
			if err != nil {
				log.Printf("failed to write block %s to response: %v.\n", blkID, err)
			}
			return nil
		} else {
			err := blockmgr.Read(repo.StoreID, blkID, &buf)
			if err != nil {
				log.Printf("failed to read block %s: %v.\n", blkID, err)
				return nil
			}
			recvBuf := buf.Bytes()
			_, err = rsp.Write(recvBuf[pos:])
			if err != nil {
				log.Printf("failed to write block %s to response: %v.\n", blkID, err)
				return nil
			}
			start += blkSize[i] - pos
			i++
			break
		}
	}

	// Always read block from the remaining block and pos=0
	for ; i < len(file.BlkIDs); i++ {
		blkID := file.BlkIDs[i]
		var buf bytes.Buffer
		if end-start+1 <= blkSize[i] {
			err := blockmgr.Read(repo.StoreID, blkID, &buf)
			if err != nil {
				log.Printf("failed to read block %s: %v.\n", blkID, err)
				return nil
			}
			recvBuf := buf.Bytes()
			_, err = rsp.Write(recvBuf[:end-start+1])
			if err != nil {
				log.Printf("failed to write block %s to response: %v.\n", blkID, err)
				return nil
			}
			break
		} else {
			err := blockmgr.Read(repo.StoreID, blkID, rsp)
			if err != nil {
				log.Printf("failed to write block %s to response: %v.\n", blkID, err)
				return nil
			}
			start += blkSize[i]
		}
	}

	return nil
}

func parseRange(byteRanges string, fileSize uint64) (uint64, uint64, bool) {
	start := strings.Index(byteRanges, "=")
	end := strings.Index(byteRanges, "-")

	if end < 0 {
		return 0, 0, false
	}

	var startByte, endByte uint64

	if start+1 == end {
		retByte, err := strconv.ParseUint(byteRanges[end+1:], 10, 64)
		if err != nil || retByte == 0 {
			return 0, 0, false
		}
		startByte = fileSize - retByte
		endByte = fileSize - 1
	} else if end+1 == len(byteRanges) {
		firstByte, err := strconv.ParseUint(byteRanges[start+1:end], 10, 64)
		if err != nil {
			return 0, 0, false
		}

		startByte = firstByte
		endByte = fileSize - 1
	} else {
		firstByte, err := strconv.ParseUint(byteRanges[start+1:end], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		lastByte, err := strconv.ParseUint(byteRanges[end+1:], 10, 64)
		if err != nil {
			return 0, 0, false
		}

		if lastByte > fileSize-1 {
			lastByte = fileSize - 1
		}

		startByte = firstByte
		endByte = lastByte
	}

	if startByte > endByte {
		return 0, 0, false
	}

	return startByte, endByte, true
}

func setCommonHeaders(rsp http.ResponseWriter, r *http.Request, operation, fileName string) {
	fileType := parseContentType(fileName)
	if fileType != "" {
		var contentType string
		if strings.Index(fileType, "text") != -1 {
			contentType = fileType + "; " + "charset=gbk"
		} else {
			contentType = contentType
		}
		rsp.Header().Set("Content-Type", contentType)
	} else {
		rsp.Header().Set("Content-Type", "application/octet-stream")
	}

	var contFileName string
	if operation == "download" || operation == "download-link" ||
		operation == "downloadblks" {
		if testFireFox(r) {
			contFileName = fmt.Sprintf("attachment;filename*=\"utf-8' '%s\"", fileName)
		} else {
			contFileName = fmt.Sprintf("attachment;filename*=\"%s\"", fileName)
		}
	} else {
		if testFireFox(r) {
			contFileName = fmt.Sprintf("inline;filename*=\"utf-8' '%s\"", fileName)
		} else {
			contFileName = fmt.Sprintf("inline;filename=\"%s\"", fileName)
		}
	}
	rsp.Header().Set("Content-Disposition", contFileName)

	if fileType != "image/jpg" {
		rsp.Header().Set("X-Content-Type-Options", "nosniff")
	}
}

func accessBlksCB(rsp http.ResponseWriter, r *http.Request) *appError {
	parts := strings.Split(r.URL.Path[1:], "/")
	if len(parts) < 3 {
		msg := "Invalid URL"
		return &appError{nil, msg, http.StatusBadRequest}
	}
	token := parts[1]
	blkID := parts[2]
	accessInfo, err := parseWebaccessInfo(rsp, token)
	if err != nil {
		return err
	}
	repoID := accessInfo.repoID
	op := accessInfo.op
	user := accessInfo.user
	id := accessInfo.objID

	if _, ok := r.Header["If-Modified-Since"]; ok {
		return &appError{nil, "", http.StatusNotModified}
	}

	now := time.Now()
	rsp.Header().Set("Last-Modified", now.Format("Mon, 2 Jan 2006 15:04:05 GMT"))
	rsp.Header().Set("Cache-Control", "max-age=3600")

	repo := repomgr.Get(repoID)
	if repo == nil {
		msg := "Bad repo id"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	exists, _ := fsmgr.Exists(repo.StoreID, id)
	if !exists {
		msg := "Invalid file id"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	if op != "downloadblks" {
		msg := "Bad access token"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	if err := doBlock(rsp, r, repo, id, user, blkID); err != nil {
		return err
	}

	return nil
}

func doBlock(rsp http.ResponseWriter, r *http.Request, repo *repomgr.Repo, fileID string,
	user string, blkID string) *appError {
	file, err := fsmgr.GetSeafile(repo.StoreID, fileID)
	if err != nil {
		msg := "Failed to get seafile"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	var found bool
	for _, id := range file.BlkIDs {
		if id == blkID {
			found = true
			break
		}
	}

	if !found {
		rsp.WriteHeader(http.StatusBadRequest)
		return nil
	}

	exists := blockmgr.Exists(repo.StoreID, blkID)
	if !exists {
		rsp.WriteHeader(http.StatusBadRequest)
		return nil
	}

	rsp.Header().Set("Access-Control-Allow-Origin", "*")
	setCommonHeaders(rsp, r, "downloadblks", blkID)

	size, err := blockmgr.Stat(repo.StoreID, blkID)
	if err != nil {
		msg := "Failed to stat block"
		return &appError{nil, msg, http.StatusBadRequest}
	}
	if size == 0 {
		rsp.WriteHeader(http.StatusOK)
		return nil
	}

	fileSize := fmt.Sprintf("%d", size)
	rsp.Header().Set("Content-Length", fileSize)

	err = blockmgr.Read(repo.StoreID, blkID, rsp)
	if err != nil {
		log.Printf("fatild to write block %s to response: %v.\n", blkID, err)
	}

	return nil
}

func accessZipCB(rsp http.ResponseWriter, r *http.Request) *appError {
	parts := strings.Split(r.URL.Path[1:], "/")
	if len(parts) != 2 {
		msg := "Invalid URL"
		return &appError{nil, msg, http.StatusBadRequest}
	}
	token := parts[1]

	accessInfo, err := parseWebaccessInfo(rsp, token)
	if err != nil {
		return err
	}

	repoID := accessInfo.repoID
	op := accessInfo.op
	user := accessInfo.user
	data := accessInfo.objID

	if op != "download-dir" && op != "download-dir-link" &&
		op != "download-multi" && op != "download-multi-link" {
		err := fmt.Errorf("wrong operation of token: %s.\n", op)
		return &appError{err, "", http.StatusInternalServerError}
	}

	if _, ok := r.Header["If-Modified-Since"]; ok {
		return &appError{nil, "", http.StatusNotModified}
	}

	now := time.Now()
	rsp.Header().Set("Last-Modified", now.Format("Mon, 2 Jan 2006 15:04:05 GMT"))
	rsp.Header().Set("Cache-Control", "max-age=3600")

	if err := downloadZipFile(rsp, r, data, repoID, user, op); err != nil {
		return err
	}

	return nil
}

func downloadZipFile(rsp http.ResponseWriter, r *http.Request, data, repoID, user, op string) *appError {
	repo := repomgr.Get(repoID)
	if repo == nil {
		msg := "Failed to get repo"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	obj := make(map[string]interface{})
	err := json.Unmarshal([]byte(data), &obj)
	if err != nil {
		err := fmt.Errorf("failed to parse obj data for zip: %v.\n", err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	ar := zip.NewWriter(rsp)
	defer ar.Close()

	if op == "download-dir" || op == "download-dir-link" {
		dirName, ok := obj["dir_name"].(string)
		if !ok || dirName == "" {
			err := fmt.Errorf("invalid download dir data: miss dir_name field.\n")
			return &appError{err, "", http.StatusInternalServerError}
		}

		objID, ok := obj["obj_id"].(string)
		if !ok || objID == "" {
			err := fmt.Errorf("invalid download dir data: miss obj_id field.\n")
			return &appError{err, "", http.StatusInternalServerError}
		}

		setCommonHeaders(rsp, r, "download", dirName)

		err := packDir(ar, repo, objID, dirName)
		if err != nil {
			log.Printf("failed to pack dir %s: %v.\n", dirName, err)
			return nil
		}
	} else {
		dirList, err := parseDirFilelist(repo, obj)
		if err != nil {
			return &appError{err, "", http.StatusInternalServerError}
		}

		now := time.Now()
		zipName := fmt.Sprintf("documents-export-%d-%d-%d.zip", now.Year(), now.Month(), now.Day())

		setCommonHeaders(rsp, r, "download", zipName)

		for _, v := range dirList {
			if fsmgr.IsDir(v.Mode) {
				if err := packDir(ar, repo, v.ID, v.Name); err != nil {
					log.Printf("failed to pack dir %s: %v.\n", v.Name, err)
					return nil
				}
			} else {
				if err := packFiles(ar, &v, repo, ""); err != nil {
					log.Printf("failed to pack file %s: %v.\n", v.Name, err)
					return nil
				}
			}
		}
	}

	return nil
}

func parseDirFilelist(repo *repomgr.Repo, obj map[string]interface{}) ([]fsmgr.SeafDirent, error) {
	parentDir, ok := obj["parent_dir"].(string)
	if !ok || parentDir == "" {
		err := fmt.Errorf("invalid download multi data, miss parent_dir field.\n")
		return nil, err
	}

	dir, err := fsmgr.GetSeafdirByPath(repo.StoreID, repo.RootID, parentDir)
	if err != nil {
		err := fmt.Errorf("failed to get dir %s repo %s.\n", parentDir, repo.StoreID)
		return nil, err
	}

	fileList, ok := obj["file_list"].([]interface{})
	if !ok || fileList == nil {
		err := fmt.Errorf("invalid download multi data, miss file_list field.\n")
		return nil, err
	}

	direntHash := make(map[string]fsmgr.SeafDirent)
	for _, v := range dir.Entries {
		direntHash[v.Name] = v
	}

	direntList := make([]fsmgr.SeafDirent, 0)

	for _, fileName := range fileList {
		name, ok := fileName.(string)
		if !ok {
			err := fmt.Errorf("invalid download multi data.\n")
			return nil, err
		}

		v, ok := direntHash[name]
		if !ok {
			err := fmt.Errorf("invalid download multi data.\n")
			return nil, err
		}

		direntList = append(direntList, v)
	}

	return direntList, nil
}

func packDir(ar *zip.Writer, repo *repomgr.Repo, dirID, dirPath string) error {
	dirent, err := fsmgr.GetSeafdir(repo.StoreID, dirID)
	if err != nil {
		err := fmt.Errorf("failed to get dir for zip: %v.\n", err)
		return err
	}

	if dirent.Entries == nil {
		fileDir := filepath.Join(dirPath)
		fileDir = strings.TrimLeft(fileDir, "/")
		_, err := ar.Create(fileDir + "/")
		if err != nil {
			err := fmt.Errorf("failed to create zip dir: %v.\n", err)
			return err
		}

		return nil
	}

	entries := dirent.Entries

	for _, v := range entries {
		fileDir := filepath.Join(dirPath, v.Name)
		fileDir = strings.TrimLeft(fileDir, "/")
		if fsmgr.IsDir(v.Mode) {
			if err := packDir(ar, repo, v.ID, fileDir); err != nil {
				return err
			}
		} else {
			if err := packFiles(ar, &v, repo, dirPath); err != nil {
				return err
			}
		}
	}

	return nil
}

func packFiles(ar *zip.Writer, dirent *fsmgr.SeafDirent, repo *repomgr.Repo, parentPath string) error {
	file, err := fsmgr.GetSeafile(repo.StoreID, dirent.ID)
	if err != nil {
		err := fmt.Errorf("failed to get seafile : %v.\n", err)
		return err
	}

	filePath := filepath.Join(parentPath, dirent.Name)
	filePath = strings.TrimLeft(filePath, "/")

	fileHeader := new(zip.FileHeader)
	fileHeader.Name = filePath
	fileHeader.Modified = time.Unix(dirent.Mtime, 0)
	fileHeader.Method = zip.Deflate
	zipFile, err := ar.CreateHeader(fileHeader)
	if err != nil {
		err := fmt.Errorf("failed to create zip file : %v.\n", err)
		return err
	}

	for _, blkID := range file.BlkIDs {
		err := blockmgr.Read(repo.StoreID, blkID, zipFile)
		if err != nil {
			return err
		}
	}

	return nil
}

type recvData struct {
	parentDir string
	tokenType string
	repoID    string
	user      string
	rstart    int64
	rend      int64
	fsize     int64
	fileNames []string
	files     []string
}

func uploadApiCB(rsp http.ResponseWriter, r *http.Request) *appError {
	fsm, err := parseUploadHeaders(rsp, r)
	if err != nil {
		return err
	}

	if err := doUpload(rsp, r, fsm, false); err != nil {
		return err
	}

	return nil
}

func uploadAjaxCB(rsp http.ResponseWriter, r *http.Request) *appError {
	fsm, err := parseUploadHeaders(rsp, r)
	if err != nil {
		return err
	}

	if err := doUpload(rsp, r, fsm, true); err != nil {
		return err
	}

	return nil
}

func doUpload(rsp http.ResponseWriter, r *http.Request, fsm *recvData, isAjax bool) *appError {
	rsp.Header().Set("Access-Control-Allow-Origin", "*")
	rsp.Header().Set("Access-Control-Allow-Headers", "x-requested-with, content-type, content-range, content-disposition, accept, origin, authorization")
	rsp.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	rsp.Header().Set("Access-Control-Max-Age", "86400")

	if r.Method == "OPTIONS" {
		rsp.WriteHeader(http.StatusOK)
		return nil
	}

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return &appError{nil, "", http.StatusBadRequest}
	}

	repoID := fsm.repoID
	user := fsm.user

	replaceStr := r.FormValue("replace")
	var replaceExisted int64
	if replaceStr != "" {
		replace, err := strconv.ParseInt(replaceStr, 10, 64)
		if err != nil || (replace != 0 && replace != 1) {
			msg := "Invalid argument.\n"
			errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
			return errReply
		}
		replaceExisted = replace
	}

	parentDir := r.FormValue("parent_dir")
	if parentDir == "" {
		msg := "Invalid URL.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
		return errReply
	}

	relativePath := r.FormValue("relative_path")
	if relativePath != "" {
		if relativePath[0] == '/' || relativePath[0] == '\\' {
			msg := "Invalid relative path"
			errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
			return errReply
		}
	}

	newParentDir := filepath.Join("/", parentDir, relativePath)
	defer clearTmpFile(fsm, newParentDir)

	if fsm.rstart >= 0 {
		if parentDir[0] != '/' {
			msg := "Invalid parent dir"
			errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
			return errReply
		}

		formFiles := r.MultipartForm.File
		files, ok := formFiles["file"]
		if !ok {
			msg := "Internal server.\n"
			errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
			errReply.Error = fmt.Errorf("failed to get file from multipart form.\n")
			return errReply
		}

		if len(files) > 1 {
			msg := "More files in one request"
			errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
			return errReply
		}

		err := writeBlockDataToTmpFile(r, fsm, formFiles, repoID, newParentDir)
		if err != nil {
			msg := "Internal error.\n"
			errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
			errReply.Error = fmt.Errorf("failed to write block data to tmp file: %v.\n", err)
			return errReply
		}

		if fsm.rend != fsm.fsize-1 {
			success := "{\"success\": true}"
			_, err := rsp.Write([]byte(success))
			if err != nil {
				log.Printf("failed to write data to response.\n")
			}
			accept, ok := r.Header["Accept"]
			if ok && strings.Index(strings.Join(accept, ""), "application/json") != -1 {
				rsp.Header().Set("Content-Type", "application/json; charset=utf-8")
			} else {
				rsp.Header().Set("Content-Type", "text/plain")
			}

			return nil
		}
	} else {
		formFiles := r.MultipartForm.File
		err := writeBlockDataToTmpFile(r, fsm, formFiles, repoID, newParentDir)
		if err != nil {
			msg := "Internal error.\n"
			errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
			errReply.Error = fmt.Errorf("failed to write block data to tmp file: %v.\n", err)
			return errReply
		}
	}

	if err := checkParentDir(rsp, repoID, parentDir); err != nil {
		return err
	}

	if !isParentMatched(fsm.parentDir, parentDir) {
		msg := "Permission denied."
		errReply := sendErrorReply(rsp, msg, http.StatusForbidden)
		return errReply
	}

	if err := checkTmpFileList(rsp, fsm.files); err != nil {
		return err
	}

	var contentLen int64
	if fsm.fsize > 0 {
		contentLen = fsm.fsize
	} else {
		lenstr := rsp.Header().Get("Content-Length")
		if lenstr == "" {
			contentLen = -1
		} else {
			tmpLen, err := strconv.ParseInt(lenstr, 10, 64)
			if err != nil {
				msg := "Internal error.\n"
				errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
				errReply.Error = fmt.Errorf("failed to parse content len: %v.\n", err)
				return errReply
			}
			contentLen = tmpLen
		}
	}

	if err := checkQuota(rsp, repoID, contentLen); err != nil {
		return err
	}

	if err := createRelativePath(rsp, repoID, parentDir, relativePath, user); err != nil {
		return err
	}

	if err := postMultiFiles(rsp, r, repoID, newParentDir, user, fsm.fileNames,
		fsm.files, replaceExisted, isAjax); err != nil {
		return err
	}

	rsp.Header().Set("Content-Type", "application/json; charset=utf-8")

	oper := "web-file-upload"
	if fsm.tokenType == "upload-link" {
		oper = "link-file-upload"
	}
	err := sendStatisticMsg(repoID, user, oper, contentLen)
	if err != nil {
		msg := "Internal error.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
		errReply.Error = fmt.Errorf("failed to send statistic message: %v.\n", err)
		return errReply
	}

	return nil
}

func clearTmpFile(fsm *recvData, parentDir string) {
	if fsm.rstart >= 0 && fsm.rend == fsm.fsize-1 {
		filePath := filepath.Join("/", parentDir, fsm.fileNames[0])
		tmpFile, err := repomgr.GetUploadTmpFile(fsm.repoID, filePath)
		if err == nil && tmpFile != "" {
			os.Remove(tmpFile)
		}
		repomgr.DelUploadTmpFile(fsm.repoID, filePath)
	}

	return
}

func sendStatisticMsg(repoID, user, eType string, bytes int64) error {
	buf := fmt.Sprintf("%s\t%s\t%s\t%d",
		eType, user, repoID, bytes)
	if _, err := rpcclient.Call("publish_event", "seaf_server.stats", buf); err != nil {
		return err
	}

	return nil
}

func parseUploadHeaders(rsp http.ResponseWriter, r *http.Request) (*recvData, *appError) {
	parts := strings.Split(r.URL.Path[1:], "/")
	if len(parts) < 2 {
		msg := "Invalid URL"
		errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
		return nil, errReply
	}
	urlOp := parts[0]
	token := parts[1]

	accessInfo, appErr := parseWebaccessInfo(rsp, token)
	if appErr != nil {
		msg := "Access denied"
		errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
		return nil, errReply
	}

	repoID := accessInfo.repoID
	op := accessInfo.op
	user := accessInfo.user
	id := accessInfo.objID

	status, err := repomgr.GetRepoStatus(repoID)
	if err != nil {
		msg := "Internal error.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
		return nil, errReply
	}
	if status != repomgr.RepoStatusNormal && status != -1 {
		msg := "Access denied"
		errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
		return nil, errReply
	}

	if op == "upload-link" {
		op = "upload"
	}
	if strings.Index(urlOp, op) != 0 {
		msg := "Access denied"
		errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
		return nil, errReply
	}

	obj := make(map[string]interface{})
	if err := json.Unmarshal([]byte(id), &obj); err != nil {
		err := fmt.Errorf("failed to decode obj data : %v.\n", err)
		return nil, &appError{err, "", http.StatusBadRequest}
	}

	parentDir, ok := obj["parent_dir"].(string)
	if !ok || parentDir == "" {
		msg := "Invalid URL"
		errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
		return nil, errReply
	}

	fsm := new(recvData)

	fsm.parentDir = parentDir
	fsm.tokenType = accessInfo.op
	fsm.repoID = repoID
	fsm.user = user
	fsm.rstart = -1
	fsm.rend = -1
	fsm.fsize = -1

	ranges := r.Header.Get("Content-Range")
	if ranges != "" {
		parseContentRange(ranges, fsm)
	}

	return fsm, nil
}

func sendErrorReply(rsp http.ResponseWriter, errMsg string, code int) *appError {
	rsp.Header().Set("Content-Type", "application/json; charset=utf-8")

	msg := fmt.Sprintf("\"error\": \"%s\"", errMsg)
	return &appError{nil, msg, code}
}

func postMultiFiles(rsp http.ResponseWriter, r *http.Request, repoID, parentDir, user string, fileNames, files []string, replace int64, isAjax bool) *appError {

	repo := repomgr.Get(repoID)
	if repo == nil {
		msg := "Failed to get repo.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
		errReply.Error = fmt.Errorf("Failed to get repo %s", repoID)
		return errReply
	}

	canonPath := getCanonPath(parentDir)

	for _, fileName := range fileNames {
		if shouldIgnoreFile(fileName) {
			msg := fmt.Sprintf("invalid fileName: %s.\n", fileName)
			errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
			return errReply
		}
	}
	if strings.Index(parentDir, "//") != -1 {
		msg := "parent_dir contains // sequence.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
		return errReply
	}

	var cryptKey *seafileCrypt
	if repo.IsEncrypted {
		key, err := parseCryptKey(rsp, repoID, user)
		if err != nil {
			if err.Code == http.StatusBadRequest {
				msg := "Repo is encrypted. Please provide password to view it."
				errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
				return errReply
			}
			errReply := sendErrorReply(rsp, "", http.StatusInternalServerError)
			errReply.Error = fmt.Errorf("failed to get crypt key: %v.\n", err.Error)
			return errReply
		}
		cryptKey = key
	}

	var ids []string
	var sizes []int64
	for _, file := range files {
		id, size, err := indexBlocks(repo.StoreID, repo.Version, file, cryptKey)
		if err != nil {
			errReply := sendErrorReply(rsp, "", http.StatusInternalServerError)
			errReply.Error = fmt.Errorf("failed to index blocks: %v.\n", err)
			return errReply
		}
		ids = append(ids, id)
		sizes = append(sizes, size)
	}

	retStr, err := postFilesAndGenCommit(fileNames, repo, user, canonPath, replace, ids, sizes)
	if err != nil {
		errReply := sendErrorReply(rsp, "", http.StatusInternalServerError)
		errReply.Error = fmt.Errorf("failed to post files and gen commit: %v.\n", err)
		return errReply
	}

	_, ok := r.Form["ret-json"]
	if ok || isAjax {
		rsp.Write([]byte(retStr))
	} else {
		var array []map[string]interface{}
		err := json.Unmarshal([]byte(retStr), &array)
		if err != nil {
			msg := "Internal error.\n"
			errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
			errReply.Error = fmt.Errorf("failed to decode data to json: %v.\n", err)
			return errReply
		}

		var ids []string
		for _, v := range array {
			id, ok := v["id"].(string)
			if !ok {
				msg := "Internal error.\n"
				errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
				errReply.Error = fmt.Errorf("failed to assert.\n")
				return errReply
			}
			ids = append(ids, id)
		}
		newIDs := strings.Join(ids, "\t")
		rsp.Write([]byte(newIDs))
	}

	return nil
}

func postFilesAndGenCommit(fileNames []string, repo *repomgr.Repo, user, canonPath string, replace int64, ids []string, sizes []int64) (string, error) {
	headCommit, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		err := fmt.Errorf("failed to get head commit for repo %s", repo.ID)
		return "", err
	}
	var names []string
	rootID, err := doPostMultiFiles(repo, headCommit.RootID, canonPath, fileNames, ids, sizes, user, replace, &names)
	if err != nil {
		err := fmt.Errorf("failed to post files to %s in repo %s.\n", canonPath, repo.ID)
		return "", err
	}

	var buf string
	if len(fileNames) > 1 {
		buf = fmt.Sprintf("Added \"%s\" and %u more files.", fileNames[0], len(fileNames)-1)
	} else {
		buf = fmt.Sprintf("Added \"%s\".", fileNames[0])
	}

	_, err = genNewCommit(repo, headCommit, rootID, user, buf)
	if err != nil {
		err := fmt.Errorf("failed to generate new commit: %v.\n", err)
		return "", err
	}

	//go mergeVirtualRepo(repo.ID, "")

	go updateRepoSize(repo.ID)

	retJson, err := formatJsonRet(names, ids, sizes)
	if err != nil {
		err := fmt.Errorf("failed to format json data.\n")
		return "", err
	}

	return string(retJson), nil
}

func formatJsonRet(nameList, idList []string, sizeList []int64) ([]byte, error) {
	var array []map[string]interface{}
	for i, _ := range nameList {
		if i >= len(idList) || i >= len(sizeList) {
			break
		}
		obj := make(map[string]interface{})
		obj["name"] = nameList[i]
		obj["id"] = idList[i]
		obj["size"] = sizeList[i]
		array = append(array, obj)
	}

	jsonstr, err := json.Marshal(array)
	if err != nil {
		err := fmt.Errorf("failed to convert array to json.\n")
		return nil, err
	}

	return jsonstr, nil
}

type jobCB func(repoID string) error
type Job struct {
	callback jobCB
	repoID   string
}

func computeRepoSize(repoID string) error {
	var size int64
	var fileCount int64

	repo := repomgr.Get(repoID)
	if repo == nil {
		err := fmt.Errorf("[scheduler] failed to get repo %s.\n", repoID)
		return err
	}
	info, err := repomgr.GetOldRepoInfo(repoID)
	if err != nil {
		err := fmt.Errorf("[scheduler] failed to get old repo info: %v.\n", err)
		return err
	}

	if info != nil && info.HeadID == repo.HeadCommitID {
		return nil
	}

	head, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		err := fmt.Errorf("[scheduler] failed to get head commit %s.\n", repo.HeadCommitID)
		return err
	}

	var oldHead *commitmgr.Commit
	if info != nil {
		commit, _ := commitmgr.Load(repo.ID, info.HeadID)
		oldHead = commit
	}

	if info != nil && oldHead != nil {
		var results []*diffEntry
		var changeSize int64
		var changeFileCount int64
		err := diffCommits(oldHead, head, &results, false)
		if err != nil {
			err := fmt.Errorf("[scheduler] failed to do diff commits: %v.\n", err)
			return err
		}

		for _, de := range results {
			if de.status == DIFF_STATUS_DELETED {
				changeSize -= de.size
				changeFileCount--
			} else if de.status == DIFF_STATUS_ADDED {
				changeSize += de.size
				changeFileCount++
			} else if de.status == DIFF_STATUS_MODIFIED {
				changeSize = changeSize + de.size + de.originSize
			}
		}
		size = info.Size + changeSize
		fileCount = info.FileCount + changeFileCount
	} else {
		info, err := fsmgr.GetFileCountInfoByPath(repo.StoreID, repo.RootID, "/")
		if err != nil {
			err := fmt.Errorf("[scheduler] failed to get file count.\n")
			return err
		}

		fileCount = info.FileCount
		size = info.Size
	}

	err = setRepoSizeAndFileCount(repoID, repo.HeadCommitID, size, fileCount)
	if err != nil {
		err := fmt.Errorf("[scheduler] failed to set repo size and file count %s: %v.\n", repoID, err)
		return err
	}

	return nil
}

func setRepoSizeAndFileCount(repoID, newHeadID string, size, fileCount int64) error {
	trans, err := seafileDB.Begin()
	if err != nil {
		err := fmt.Errorf("failed to start transaction: %v.\n", err)
		return err
	}

	var headID string
	sqlStr := "SELECT head_id FROM RepoSize WHERE repo_id=?"

	row := trans.QueryRow(sqlStr, repoID)
	if err := row.Scan(&headID); err != nil {
		if err != sql.ErrNoRows {
			trans.Rollback()
			return err
		}
	}

	if headID == "" {
		sqlStr := "INSERT INTO RepoSize (repo_id, size, head_id) VALUES (?, ?, ?)"
		_, err = trans.Exec(sqlStr, repoID, size, newHeadID)
		if err != nil {
			trans.Rollback()
			return err
		}
	} else {
		sqlStr = "UPDATE RepoSize SET size = ?, head_id = ? WHERE repo_id = ?"
		_, err = trans.Exec(sqlStr, size, newHeadID, repoID)
		if err != nil {
			trans.Rollback()
			return err
		}
	}

	var exist int
	sqlStr = "SELECT 1 FROM RepoFileCount WHERE repo_id=?"
	row = trans.QueryRow(sqlStr, repoID)
	if err := row.Scan(&exist); err != nil {
		if err != sql.ErrNoRows {
			trans.Rollback()
			return err
		}
	}

	if exist != 0 {
		sqlStr := "UPDATE RepoFileCount SET file_count=? WHERE repo_id=?"
		_, err = trans.Exec(sqlStr, fileCount, repoID)
		if err != nil {
			trans.Rollback()
			return err
		}
	} else {
		sqlStr := "INSERT INTO RepoFileCount (repo_id,file_count) VALUES (?,?)"
		_, err = trans.Exec(sqlStr, repoID, fileCount)
		if err != nil {
			trans.Rollback()
			return err
		}
	}

	trans.Commit()

	return nil
}

var jobs = make(chan Job, 10)

func updateRepoSize(repoID string) {
	job := Job{computeRepoSize, repoID}
	jobs <- job
}

// need to start a go routine
func createWorkerPool(n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go worker(&wg)
	}
	wg.Wait()
}

func worker(wg *sync.WaitGroup) {
	for {
		select {
		case job := <-jobs:
			if job.callback != nil {
				err := job.callback(job.repoID)
				if err != nil {
					log.Printf("failed to call jobs: %v.\n", err)
				}
			}
		default:
		}
	}
	wg.Done()
}

func mergeVirtualRepo(repoID, excludeRepo string) {
	virtual, err := repomgr.IsVirtualRepo(repoID)
	if err != nil {
		return
	}

	if virtual {
		mergeRepo(repoID)
		return
	}

	vRepos, _ := repomgr.GetVirtualRepoIDsByOrigin(repoID)
	for _, id := range vRepos {
		if id == excludeRepo {
			continue
		}

		mergeRepo(id)
	}

	return
}

func mergeRepo(repoID string) error {
	repo := repomgr.Get(repoID)
	if repo == nil {
		err := fmt.Errorf("failed to get virt repo %.10s.\n", repoID)
		return err
	}
	vInfo := repo.VirtualInfo
	if vInfo == nil {
		return nil
	}
	origRepo := repomgr.Get(vInfo.OriginRepoID)
	if origRepo == nil {
		err := fmt.Errorf("failed to get orig repo %.10s.\n", repoID)
		return err
	}

	head, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		err := fmt.Errorf("failed to get commit %s:%.8s.\n", repo.ID, repo.HeadCommitID)
		return err
	}
	origHead, err := commitmgr.Load(origRepo.ID, origRepo.HeadCommitID)
	if err != nil {
		err := fmt.Errorf("failed to get commit %s:%.8s.\n", origRepo.ID, origRepo.HeadCommitID)
		return err
	}

	var origRoot string
	origRoot, _ = fsmgr.GetSeafdirIDByPath(origRepo.StoreID, origHead.RootID, vInfo.Path)
	if origRoot == "" {
		newPath, _ := handleMissingVirtualRepo(origRepo, origHead, vInfo)
		if newPath != "" {
			origRoot, _ = fsmgr.GetSeafdirIDByPath(origRepo.StoreID, origHead.RootID, newPath)
		}
		if origRoot == "" {
			err := fmt.Errorf("path %s not found in origin repo %.8s, delete or rename virtual repo %.8s\n", vInfo.Path, vInfo.OriginRepoID, repoID)
			return err
		}
	}

	base, err := commitmgr.Load(origRepo.ID, vInfo.BaseCommitID)
	if err != nil {
		err := fmt.Errorf("failed to get commit %s:%.8s.\n", origRepo.ID, vInfo.BaseCommitID)
		return err
	}

	root := head.RootID
	baseRoot, _ := fsmgr.GetSeafdirIDByPath(origRepo.StoreID, base.RootID, vInfo.Path)
	if baseRoot == "" {
		err := fmt.Errorf("cannot find seafdir for repo %.10s path %s.\n", vInfo.OriginRepoID, vInfo.Path)
		return err
	}

	if root == origRoot {
	} else if baseRoot == root {
		_, err := updateDir(repoID, "/", origRoot, origHead.CreatorName, head.CommitID)
		if err != nil {
			err := fmt.Errorf("failed to update root of virtual repo %.10s.\n", repoID)
			return err
		}
		repomgr.SetVirtualRepoBaseCommitPath(repo.ID, origRepo.HeadCommitID, vInfo.Path)
	} else if baseRoot == origRoot {
		_, err := updateDir(vInfo.OriginRepoID, vInfo.Path, root, head.CreatorName, origHead.CommitID)
		if err != nil {
			err := fmt.Errorf("failed to update origin repo%.10s path %s.\n", vInfo.OriginRepoID, vInfo.Path)
			return err
		}
		repomgr.SetVirtualRepoBaseCommitPath(repo.ID, origRepo.HeadCommitID, vInfo.Path)
		CleanupVirtualRepos(vInfo.OriginRepoID)
		mergeVirtualRepo(vInfo.OriginRepoID, repoID)
	} else {
		var roots []string
		roots = append(roots, baseRoot)
		roots = append(roots, origRoot)
		roots = append(roots, root)
		opt := new(mergeOptions)
		opt.nWays = 3
		opt.remoteRepoID = repoID
		opt.remoteHead = head.CommitID
		opt.doMerge = true

		err := mergeTrees(repo.StoreID, 3, roots, opt)
		if err != nil {
			err := fmt.Errorf("failed to merge.\n")
			return err
		}

		_, err = updateDir(repoID, "/", opt.mergedRoot, origHead.CreatorName, head.CommitID)
		if err != nil {
			err := fmt.Errorf("failed to update root of virtual repo %.10s.\n", repoID)
			return err
		}

		newBaseCommit, err := updateDir(vInfo.OriginRepoID, vInfo.Path, opt.mergedRoot, head.CreatorName, origHead.CommitID)
		if err != nil {
			err := fmt.Errorf("failed to update origin repo %.10s path %s.\n", vInfo.OriginRepoID, vInfo.Path)
			return err
		}
		repomgr.SetVirtualRepoBaseCommitPath(repo.ID, newBaseCommit, vInfo.Path)
		CleanupVirtualRepos(vInfo.OriginRepoID)
		mergeVirtualRepo(vInfo.OriginRepoID, repoID)
	}

	return nil
}

func CleanupVirtualRepos(repoID string) error {
	repo := repomgr.Get(repoID)
	if repo == nil {
		err := fmt.Errorf("failed to get repo %.10s.\n", repoID)
		return err
	}

	head, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		err := fmt.Errorf("failed to load commit %s/%s : %v.\n", repo.ID, repo.HeadCommitID, err)
		return err
	}

	vRepos, err := repomgr.GetVirtualRepoInfoByOrigin(repoID)
	if err != nil {
		err := fmt.Errorf("failed to get virtual repo ids by origin repo %.10s.\n", repoID)
		return err
	}
	for _, vInfo := range vRepos {
		_, err := fsmgr.GetSeafdirByPath(repo.StoreID, head.RootID, vInfo.Path)
		if err != nil {
			if err == fsmgr.PathNoExist {
				handleMissingVirtualRepo(repo, head, vInfo)
			}
		}
	}

	return nil
}

func updateDir(repoID, dirPath, newDirID, user, headID string) (string, error) {
	repo := repomgr.Get(repoID)
	if repo == nil {
		err := fmt.Errorf("failed to get repo %.10s.\n", repoID)
		return "", err
	}

	var base string
	if headID == "" {
		base = repo.HeadCommitID
	} else {
		base = headID
	}

	headCommit, err := commitmgr.Load(repo.ID, base)
	if err != nil {
		err := fmt.Errorf("failed to get head commit for repo %s", repo.ID)
		return "", err
	}

	if dirPath == "/" {
		commitDesc := genCommitDesc(repo, newDirID, headCommit.RootID)
		if commitDesc == "" {
			commitDesc = fmt.Sprintf("Auto merge by system")
		}
		newCommitID, err := genNewCommit(repo, headCommit, newDirID, user, commitDesc)
		if err != nil {
			err := fmt.Errorf("failed to generate new commit: %v.\n", err)
			return "", err
		}
		return newCommitID, nil
	}

	parent := filepath.Dir(dirPath)
	canonPath := getCanonPath(parent)
	dirName := filepath.Base(dirPath)

	dir, err := fsmgr.GetSeafdirByPath(repo.StoreID, headCommit.RootID, canonPath)
	if err != nil {
		err := fmt.Errorf("dir %s doesn't exist in repo %s.\n", canonPath, repo.StoreID)
		return "", err
	}
	var exists bool
	for _, de := range dir.Entries {
		if de.Name == dirName {
			exists = true
		}
	}
	if !exists {
		err := fmt.Errorf("file %s doesn't exist in repo %s.\n", dirName, repo.StoreID)
		return "", err
	}
	newDent := new(fsmgr.SeafDirent)
	newDent.ID = newDirID
	newDent.Mode = (syscall.S_IFDIR | 0644)
	newDent.Mtime = time.Now().Unix()
	newDent.Name = dirName

	rootID, err := doPutFile(repo, headCommit.RootID, canonPath, newDent)
	if err != nil || rootID == "" {
		err := fmt.Errorf("failed to put file.\n", err)
		return "", err
	}

	commitDesc := genCommitDesc(repo, rootID, headCommit.RootID)
	if commitDesc == "" {
		commitDesc = fmt.Sprintf("Auto merge by system")
	}

	newCommitID, err := genNewCommit(repo, headCommit, rootID, user, commitDesc)
	if err != nil {
		err := fmt.Errorf("failed to generate new commit: %v.\n", err)
		return "", err
	}

	return newCommitID, nil
}

func doPutFile(repo *repomgr.Repo, rootID, parentDir string, dent *fsmgr.SeafDirent) (string, error) {
	if strings.Index(parentDir, "/") == 0 {
		parentDir = parentDir[1:]
	}

	return putFileRecursive(repo, rootID, parentDir, dent)
}

func putFileRecursive(repo *repomgr.Repo, dirID, toPath string, newDent *fsmgr.SeafDirent) (string, error) {
	olddir, err := fsmgr.GetSeafdir(repo.StoreID, dirID)
	if err != nil {
		err := fmt.Errorf("failed to get dir.\n")
		return "", err
	}
	entries := olddir.Entries
	sort.Sort(Dirents(entries))

	var ret string

	if toPath == "" {
		var newEntries []fsmgr.SeafDirent
		for _, dent := range entries {
			if dent.Name == newDent.Name {
				newEntries = append(newEntries, *newDent)
			} else {
				newEntries = append(newEntries, dent)
			}
		}

		newDir := new(fsmgr.SeafDir)
		newDir.Version = 1
		newDir.Entries = newEntries
		jsonstr, err := json.Marshal(newDir)
		if err != nil {
			err := fmt.Errorf("failed to convert seadir to json.\n")
			return "", err
		}
		checkSum := sha1.Sum(jsonstr)
		id := hex.EncodeToString(checkSum[:])
		err = fsmgr.SaveSeafdir(repo.StoreID, id, newDir)
		if err != nil {
			err := fmt.Errorf("failed to save seafdir %s/%s.\n", repo.ID, id)
			return "", err
		}

		return id, nil
	}

	var remain string
	if slash := strings.Index(toPath, "/"); slash >= 0 {
		remain = toPath[slash+1:]
	}

	for _, dent := range entries {
		if dent.Name != toPath {
			continue
		}
		id, err := putFileRecursive(repo, dent.ID, remain, newDent)
		if err != nil {
			err := fmt.Errorf("failed to put dirent %s: %v.\n", dent.Name, err)
			return "", err
		}
		if id != "" {
			dent.ID = id
			dent.Mtime = time.Now().Unix()
		}
		ret = id
		break
	}

	if ret != "" {
		newDir := new(fsmgr.SeafDir)
		newDir.Version = 1
		newDir.Entries = entries
		jsonstr, err := json.Marshal(newDir)
		if err != nil {
			err := fmt.Errorf("failed to convert seafdir to json.\n")
			return "", err
		}
		checkSum := sha1.Sum(jsonstr)
		id := hex.EncodeToString(checkSum[:])
		err = fsmgr.SaveSeafdir(repo.StoreID, id, newDir)
		if err != nil {
			err := fmt.Errorf("failed to save seafdir %s/%s.\n", repo.ID, id)
			return "", err
		}
		ret = id
	}

	return ret, nil
}

func getCanonPath(p string) string {
	formatPath := strings.Replace(p, "\\", "/", -1)
	return filepath.Join(formatPath)
}

func handleMissingVirtualRepo(repo *repomgr.Repo, head *commitmgr.Commit, vInfo *repomgr.VRepoInfo) (string, error) {
	parent, err := commitmgr.Load(head.RepoID, head.ParentID)
	if err != nil {
		err := fmt.Errorf("failed to load commit %s/%s : %v.\n", head.RepoID, head.ParentID, err)
		return "", err
	}

	var results []*diffEntry
	err = diffCommits(parent, head, &results, true)
	if err != nil {
		err := fmt.Errorf("failed to diff commits.\n")
		return "", err
	}

	parPath := vInfo.Path
	var isRenamed bool
	var subPath string
	var returnPath string
	for {
		var newPath string
		oldDirID, _ := fsmgr.GetSeafdirIDByPath(repo.StoreID, parent.RootID, parPath)
		if oldDirID == "" {

			err := fmt.Errorf("failed to find %s under commit %s in repo %s.\n", parPath, parent.CommitID, repo.StoreID)
			return "", err
		}

		for _, de := range results {
			if de.status == DIFF_STATUS_DIR_RENAMED {
				if de.dirID == oldDirID {
					if subPath != "" {
						newPath = filepath.Join("/", de.newName, subPath)
					} else {
						newPath = filepath.Join("/", de.newName)
					}
					repomgr.SetVirtualRepoBaseCommitPath(vInfo.RepoID, head.CommitID, newPath)
					returnPath = newPath
					if subPath == "" {
						newName := filepath.Base(newPath)
						err := editRepo(repo.ID, newName, "Changed library name", "")
						if err != nil {
							log.Printf("falied to rename repo %s.\n", newName)
						}
					}
					isRenamed = true
					break
				}
			}
		}

		if isRenamed {
			break
		}

		slash := strings.Index(parPath, "/")
		if slash <= 0 {
			break
		}
		subPath = filepath.Base(parPath)
		parPath = filepath.Dir(parPath)
	}

	if !isRenamed {
		repomgr.DelVirtualRepo(vInfo.RepoID, cloudMode)
	}

	return returnPath, nil
}

func editRepo(repoID, name, desc, user string) error {
	if name == "" && desc == "" {
		err := fmt.Errorf("at least one argument should be non-null.\n")
		return err
	}

retry:
	repo := repomgr.Get(repoID)
	if repo == nil {
		err := fmt.Errorf("no such library")
		return err
	}
	if name == "" {
		name = repo.Name
	}
	if desc == "" {
		desc = repo.Desc
	}

	parent, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		err := fmt.Errorf("failed to get commit %s:%s.\n", repo.ID, repo.HeadCommitID)
		return err
	}

	if user == "" {
		user = parent.CreatorName
	}

	commit := newCommit(repo, parent.CommitID, parent.RootID, user, "Changed library name or description")
	commit.RepoName = name
	commit.RepoDesc = desc

	err = commitmgr.Save(commit)
	if err != nil {
		err := fmt.Errorf("failed to add commit: %v.\n", err)
		return err
	}

	err = updateBranch(repoID, commit.CommitID, parent.CommitID)
	if err != nil {
		goto retry
	}

	updateRepoInfo(repoID, commit.CommitID)

	return nil
}

func updateRepoInfo(repoID, commitID string) error {
	head, err := commitmgr.Load(repoID, commitID)
	if err != nil {
		err := fmt.Errorf("failed to get commit %s:%s.\n", repoID, commitID)
		return err
	}

	repomgr.SetRepoCommitToDb(repoID, head.RepoName, head.Ctime, head.Version, head.Encrypted, head.CreatorName)

	return nil
}

func setRepoCommitToDb(repoID, repoName, string, updateTime int64, version int, isEncrypted string, lastModifier string) error {
	var exists int
	var encrypted int

	sqlStr := "SELECT 1 FROM RepoInfo WHERE repo_id=?"
	row := seafileDB.QueryRow(sqlStr, repoID)
	if err := row.Scan(&exists); err != nil {
		if err != sql.ErrNoRows {
			return err
		}
	}
	if updateTime == 0 {
		updateTime = time.Now().Unix()
	}

	if isEncrypted == "true" {
		encrypted = 1
	}

	if exists == 1 {
		sqlStr := "UPDATE RepoInfo SET name=?, update_time=?, version=?, is_encrypted=?, " +
			"last_modifier=? WHERE repo_id=?"
		if _, err := seafileDB.Exec(sqlStr, repoName, updateTime, version, encrypted, lastModifier, repoID); err != nil {
			return err
		}
	} else {
		sqlStr := "INSERT INTO RepoInfo (repo_id, name, update_time, version, is_encrypted, last_modifier) " +
			"VALUES (?, ?, ?, ?, ?, ?)"
		if _, err := seafileDB.Exec(sqlStr, repoID, repoName, updateTime, version, encrypted, lastModifier); err != nil {
			return err
		}
	}

	return nil
}

func setVirtualRepoBaseCommitPath(repoID, baseCommitID, newPath string) error {
	sqlStr := "UPDATE VirtualRepo SET base_commit=?, path=? WHERE repo_id=?"
	if _, err := seafileDB.Exec(sqlStr, baseCommitID, newPath, repoID); err != nil {
		return err
	}
	return nil
}

func getVirtualRepoIDsByOrigin(repoID string) ([]string, error) {
	sqlStr := "SELECT repo_id FROM VirtualRepo WHERE origin_repo=?"

	var id string
	var ids []string
	row, err := seafileDB.Query(sqlStr, repoID)
	if err != nil {
		return nil, err
	}
	for row.Next() {
		if err := row.Scan(&id); err != nil {
			if err != sql.ErrNoRows {
				return nil, err
			}
		}
		ids = append(ids, id)
	}

	return ids, nil
}

func genNewCommit(repo *repomgr.Repo, base *commitmgr.Commit, newRoot, user, desc string) (string, error) {
	var mergeDesc string
	var retryCnt int
	repoID := repo.ID
	commit := newCommit(repo, base.CommitID, newRoot, user, desc)
	err := commitmgr.Save(commit)
	if err != nil {
		err := fmt.Errorf("failed to add commit: %v.\n", err)
		return "", err
	}

retry:
	var mergedCommit *commitmgr.Commit
	currentHead, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		err := fmt.Errorf("failed to get head commit for repo %s", repoID)
		return "", err
	}
	if base.CommitID != currentHead.CommitID {
		var roots []string
		roots = append(roots, base.RootID)
		roots = append(roots, currentHead.RootID)
		roots = append(roots, newRoot)
		opt := new(mergeOptions)
		opt.nWays = 3
		opt.remoteRepoID = repoID
		opt.remoteHead = commit.CommitID
		opt.doMerge = true

		err := mergeTrees(repo.StoreID, 3, roots, opt)
		if err != nil {
			err := fmt.Errorf("failed to merge.\n")
			return "", err
		}

		if !opt.conflict {
			mergeDesc = fmt.Sprintf("Auto merge by system")
		} else {
			mergeDesc = genMergeDesc(repo, opt.mergedRoot, currentHead.RootID, newRoot)
			if mergeDesc == "" {
				mergeDesc = fmt.Sprintf("Auto merge by system")
			}
		}

		mergedCommit = newCommit(repo, currentHead.CommitID, opt.mergedRoot, user, mergeDesc)
		mergedCommit.SecondParentID = commit.CommitID
		mergedCommit.NewMerge = 1
		if opt.conflict {
			mergedCommit.Conflict = 1
		}

		err = commitmgr.Save(commit)
		if err != nil {
			err := fmt.Errorf("failed to add commit: %v.\n", err)
			return "", err
		}
	} else {
		mergedCommit = commit
	}

	err = updateBranch(repoID, mergedCommit.CommitID, currentHead.CommitID)
	if err != nil {
		if retryCnt < 3 {
			random := rand.Intn(10) + 1
			time.Sleep(time.Duration(random*100) * time.Millisecond)
			repo = repomgr.Get(repoID)
			if repo == nil {
				err := fmt.Errorf("repo %s doesn't exist.\n", repoID)
				return "", err
			}
			retryCnt++
			goto retry
		} else {
			err := fmt.Errorf("stop updating repo %s after 3 retries.\n", repoID)
			return "", err
		}
	}

	return mergedCommit.CommitID, nil
}

func genCommitDesc(repo *repomgr.Repo, root, parentRoot string) string {
	var results []*diffEntry
	err := diffCommitRoots(repo.StoreID, parentRoot, root, &results, true)
	if err != nil {
		return ""
	}

	desc := diffResultsToDesc(results)

	return desc
}

func genMergeDesc(repo *repomgr.Repo, mergedRoot, p1Root, p2Root string) string {
	var results []*diffEntry
	err := diffMergeRoots(repo.StoreID, mergedRoot, p1Root, p2Root, &results, true)
	if err != nil {
		return ""
	}

	desc := diffResultsToDesc(results)

	return desc
}

func updateBranch(repoID, newCommitID, oldCommitID string) error {
	var commitID string
	name := "master"
	sqlStr := "SELECT commit_id FROM Branch WHERE name = ? AND repo_id = ?"

	trans, err := seafileDB.Begin()
	if err != nil {
		err := fmt.Errorf("failed to start transaction: %v.\n", err)
		return err
	}
	row := trans.QueryRow(sqlStr, name, repoID)
	if err := row.Scan(&commitID); err != nil {
		if err != sql.ErrNoRows {
			trans.Rollback()
			return err
		}
	}
	if oldCommitID != commitID {
		trans.Rollback()
		err := fmt.Errorf("head commit id has changed.\n")
		return err
	}

	sqlStr = "UPDATE Branch SET commit_id = ? WHERE name = ? AND repo_id = ?"
	_, err = trans.Exec(sqlStr, newCommitID, name, repoID)
	if err != nil {
		trans.Rollback()
		return err
	}

	trans.Commit()

	return nil
}

func newCommit(repo *repomgr.Repo, parentID string, newRoot, user, desc string) *commitmgr.Commit {
	commit := new(commitmgr.Commit)
	commit.RepoID = repo.ID
	commit.RootID = newRoot
	commit.Desc = desc
	commit.CreatorName = user
	commit.CreatorID = "0000000000000000000000000000000000000000"
	commit.Ctime = time.Now().Unix()
	commit.CommitID = computeCommitID(commit)
	commit.ParentID = parentID
	commit.RepoName = repo.Name
	if repo.IsEncrypted {
		commit.Encrypted = "true"
		commit.EncVersion = repo.EncVersion
		if repo.EncVersion == 1 {
			commit.Magic = repo.Magic
		} else if repo.EncVersion == 2 {
			commit.Magic = repo.Magic
			commit.RandomKey = repo.RandomKey
		} else if repo.EncVersion == 3 {
			commit.Magic = repo.Magic
			commit.RandomKey = repo.RandomKey
			commit.Salt = repo.Salt
		}
	} else {
		commit.Encrypted = "false"
	}
	commit.Version = repo.Version

	return commit
}

func computeCommitID(commit *commitmgr.Commit) string {
	hash := sha1.New()
	hash.Write([]byte(commit.RootID))
	hash.Write([]byte(commit.CreatorID))
	hash.Write([]byte(commit.CreatorName))
	hash.Write([]byte(commit.Desc))
	tmpBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(tmpBuf, uint64(commit.Ctime))
	hash.Write(tmpBuf)

	checkSum := hash.Sum(nil)
	id := hex.EncodeToString(checkSum[:])

	return id
}

func doPostMultiFiles(repo *repomgr.Repo, rootID, parentDir string, fileNames, ids []string, sizes []int64, user string, replace int64, names *[]string) (string, error) {
	var dents []*fsmgr.SeafDirent
	for i, name := range fileNames {
		if i > len(ids)-1 || i > len(sizes)-1 {
			break
		}
		dent := new(fsmgr.SeafDirent)
		dent.Name = name
		dent.ID = ids[i]
		dent.Size = sizes[i]
		dent.Mtime = time.Now().Unix()
		dent.Mode = (syscall.S_IFREG | 0644)
		dents = append(dents, dent)
	}
	if parentDir[0] == '/' {
		parentDir = parentDir[1:]
	}

	id, err := postMultiFilesRecursive(repo, rootID, parentDir, user, dents, replace, names)
	if err != nil {
		err := fmt.Errorf("failed to post multi files: %v.\n", err)
		return "", err
	}

	return id, nil
}

func postMultiFilesRecursive(repo *repomgr.Repo, dirID, toPath, user string, dents []*fsmgr.SeafDirent, replace int64, names *[]string) (string, error) {
	olddir, err := fsmgr.GetSeafdir(repo.StoreID, dirID)
	if err != nil {
		err := fmt.Errorf("failed to get dir.\n")
		return "", err
	}
	sort.Sort(Dirents(olddir.Entries))

	var ret string

	if toPath == "" {
		err := addNewEntries(repo, user, &olddir.Entries, dents, replace, names)
		if err != nil {
			err := fmt.Errorf("failed to add new entries: %v\n", err)
			return "", err
		}
		newdir := new(fsmgr.SeafDir)
		newdir.Version = 1
		newdir.Entries = olddir.Entries
		jsonstr, err := json.Marshal(newdir)
		if err != nil {
			err := fmt.Errorf("failed to convert seafdir to json.\n")
			return "", err
		}
		checksum := sha1.Sum(jsonstr)
		id := hex.EncodeToString(checksum[:])
		err = fsmgr.SaveSeafdir(repo.StoreID, id, newdir)
		if err != nil {
			err := fmt.Errorf("failed to save seafdir %s/%s.\n", repo.ID, id)
			return "", err
		}

		return id, nil
	}

	var remain string
	if slash := strings.Index(toPath, "/"); slash >= 0 {
		remain = toPath[slash+1:]
	}

	entries := olddir.Entries
	for i, dent := range entries {
		if dent.Name != toPath {
			continue
		}

		id, err := postMultiFilesRecursive(repo, dent.ID, remain, user, dents, replace, names)
		if err != nil {
			err := fmt.Errorf("failed to post dirent %s: %v.\n", dent.Name, err)
			return "", err
		}
		ret = id
		if id != "" {
			entries[i].ID = id
			entries[i].Mtime = time.Now().Unix()
		}
		break
	}

	if ret != "" {
		newDir := new(fsmgr.SeafDir)
		newDir.Version = 1
		newDir.Entries = entries
		jsonstr, err := json.Marshal(newDir)
		if err != nil {
			err := fmt.Errorf("failed to convert seafdir to json.\n")
			return "", err
		}
		checkSum := sha1.Sum(jsonstr)
		id := hex.EncodeToString(checkSum[:])
		err = fsmgr.SaveSeafdir(repo.StoreID, id, newDir)
		if err != nil {
			err := fmt.Errorf("failed to save seafdir %s/%s.\n", repo.ID, id)
			return "", err
		}
		ret = id
	}

	return ret, nil
}

func addNewEntries(repo *repomgr.Repo, user string, entries *[]fsmgr.SeafDirent, dents []*fsmgr.SeafDirent, replaceExisted int64, names *[]string) error {
	for _, dent := range dents {
		var replace bool
		var uniqueName string
		if replaceExisted != 0 {
			for _, entry := range *entries {
				if entry.Name == dent.Name {
					replace = true
					break
				}
			}
		}

		if replace {
			uniqueName = dent.Name
		} else {
			uniqueName = genUniqueName(dent.Name, *entries)
		}
		if uniqueName != "" {
			newDent := new(fsmgr.SeafDirent)
			newDent.Name = uniqueName
			newDent.ID = dent.ID
			newDent.Size = dent.Size
			newDent.Mtime = dent.Mtime
			newDent.Mode = dent.Mode
			newDent.Modifier = user
			*entries = append(*entries, *newDent)
			*names = append(*names, uniqueName)
		} else {
			err := fmt.Errorf("failed to generate unique name for %s.\n", dent.Name)
			return err
		}
	}

	sort.Sort(Dirents(*entries))

	return nil
}

type Dirents []fsmgr.SeafDirent

func (d Dirents) Less(i, j int) bool {
	return d[i].Name < d[j].Name
}

func (d Dirents) Swap(i, j int) {
	d[i], d[j] = d[j], d[i]
}
func (d Dirents) Len() int {
	return len(d)
}

func genUniqueName(fileName string, entries []fsmgr.SeafDirent) string {
	var uniqueName string
	var name string
	i := 1
	dot := strings.Index(fileName, ".")
	if dot < 0 {
		name = fileName
	} else {
		name = fileName[:dot]
	}
	uniqueName = fileName
	for nameExists(entries, uniqueName) && i <= 100 {
		if dot < 0 {
			uniqueName = fmt.Sprintf("%s (%d)", name, i)
		} else {
			uniqueName = fmt.Sprintf("%s (%d).%s", name, i, fileName[dot+1:])
		}
		i++
	}

	if i <= 100 {
		return uniqueName
	}

	return ""
}

func nameExists(entries []fsmgr.SeafDirent, fileName string) bool {
	for _, entry := range entries {
		if entry.Name == fileName {
			return true
		}
	}

	return false
}

func shouldIgnoreFile(fileName string) bool {
	if !utf8.ValidString(fileName) {
		log.Printf("file name %s contains non-UTF8 characters, skip.\n", fileName)
		return true
	}

	if len(fileName) >= 256 {
		return true
	}

	if strings.Index(fileName, "/") != -1 {
		return true
	}

	return false
}

func indexBlocks(repoID string, version int, filePath string, cryptKey *seafileCrypt) (string, int64, error) {

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		err := fmt.Errorf("failed to stat file %s: %v.\n", filePath, err)
		return "", -1, err
	}

	blkIDs, err := splitFile(repoID, version, filePath, fileInfo.Size(), cryptKey)
	if err != nil {
		err := fmt.Errorf("failed to split file: %v.\n", err)
		return "", -1, err
	}

	fileID, err := writeSeafile(repoID, version, fileInfo.Size(), blkIDs)
	if err != nil {
		err := fmt.Errorf("failed to write seafile: %v.\n", err)
		return "", -1, err
	}

	return fileID, fileInfo.Size(), nil
}

func writeSeafile(repoID string, version int, fileSize int64, blkIDs []string) (string, error) {
	seafile := new(fsmgr.Seafile)
	seafile.Version = version
	seafile.FileSize = uint64(fileSize)
	seafile.BlkIDs = blkIDs

	jsonstr, err := json.Marshal(seafile)
	if err != nil {
		err := fmt.Errorf("failed to convert seafile to json.\n")
		return "", err
	}
	checkSum := sha1.Sum(jsonstr)
	fileID := hex.EncodeToString(checkSum[:])

	err = fsmgr.SaveSeafile(repoID, fileID, seafile)
	if err != nil {
		err := fmt.Errorf("failed to save seafile %s/%s.\n", repoID, fileID)
		return "", err
	}

	return fileID, nil
}

func splitFile(repoID string, version int, filePath string, fileSize int64, cryptKey *seafileCrypt) ([]string, error) {

	var blkSize int64
	var offset int64
	var left int64
	var num int
	ch := make(chan *chunkingData)
	defer close(ch)

	left = fileSize
	for left > 0 {
		blkSize = 1 << 20
		/*
			if uint64(left) >= options.fixedBlockSize {
				blkSize = int64(options.fixedBlockSize)
			} else {
				blkSize = left
			}
		*/
		num++
		go chunkingWorker(ch, repoID, filePath, offset, blkSize, cryptKey)

		left -= blkSize
		offset += blkSize
	}

	blkIDs := make([]string, num)
	for ; num > 0; num-- {
		chunk := <-ch
		if chunk.err != nil {
			err := fmt.Errorf("failed to chunk: %v.\n", chunk.err)
			return nil, err
		}
		//blkIDs = append(blkIDs, chunk.blkID)
		blkIDs[chunk.idx] = chunk.blkID
	}

	return blkIDs, nil
}

type chunkingData struct {
	idx   int64
	blkID string
	err   error
}

func chunkingWorker(ch chan *chunkingData, repoID string, filePath string, offset int64, blkSize int64, cryptKey *seafileCrypt) {
	chunk := new(chunkingData)
	file, err := os.Open(filePath)
	if err != nil {
		chunk.err = fmt.Errorf("failed to open file %s: %v.\n", filePath, err)
		ch <- chunk
		return
	}

	_, err = file.Seek(offset, 0)
	if err != nil {
		chunk.err = fmt.Errorf("failed to seek file %s: %v.\n", filePath, err)
		ch <- chunk
		return
	}
	buf := make([]byte, blkSize)
	n, err := file.Read(buf)
	if err != nil {
		chunk.err = fmt.Errorf("failed to seek file %s: %v.\n", filePath, err)
		ch <- chunk
		return
	}
	buf = buf[:n]

	blkID, err := writeChunk(repoID, buf, blkSize, cryptKey)
	if err != nil {
		chunk.err = fmt.Errorf("failed to write chunk: %v.\n", err)
		ch <- chunk
		return
	}

	idx := offset / (1 << 20) //options.fixedBlockSize
	chunk.idx = idx
	chunk.blkID = blkID
	ch <- chunk
	return
}

func writeChunk(repoID string, input []byte, blkSize int64, cryptKey *seafileCrypt) (string, error) {
	var blkID string
	if cryptKey != nil && blkSize > 0 {
		encKey := cryptKey.key
		encIv := cryptKey.iv
		encoded, err := encrypt(input, encKey, encIv)
		if err != nil {
			err := fmt.Errorf("failed to encrypt block: %v.\n", err)
			return "", err
		}
		checkSum := sha1.Sum(encoded)
		blkID = hex.EncodeToString(checkSum[:])
		reader := bytes.NewReader(encoded)
		err = blockmgr.Write(repoID, blkID, reader)
		if err != nil {
			err := fmt.Errorf("failed to write block: %v.\n", err)
			return "", err
		}
	} else {
		checkSum := sha1.Sum(input)
		blkID = hex.EncodeToString(checkSum[:])
		reader := bytes.NewReader(input)
		err := blockmgr.Write(repoID, blkID, reader)
		if err != nil {
			err := fmt.Errorf("failed to write block: %v.\n", err)
			return "", err
		}
	}

	return blkID, nil
}

func checkQuota(rsp http.ResponseWriter, repoID string, contentLen int64) *appError {
	ret, err := rpcclient.Call("check_quota", repoID, contentLen)
	if err != nil {
		msg := "Internal error.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
		errReply.Error = fmt.Errorf("failed to call check quota rpc: %v.\n", err)
		return errReply
	}
	if int(ret.(float64)) != 0 {
		msg := "Out of quota.\n"
		errReply := sendErrorReply(rsp, msg, 443)
		return errReply
	}

	return nil
}

func createRelativePath(rsp http.ResponseWriter, repoID, parentDir, relativePath, user string) *appError {
	if relativePath == "" {
		return nil
	}

	err := mkdirWithParents(repoID, parentDir, relativePath, user)
	if err != nil {
		msg := "Internal error.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
		errReply.Error = fmt.Errorf("[upload folder] %v.\n", err)
		return errReply
	}

	return nil
}

func mkdirWithParents(repoID, parentDir, newDirPath, user string) error {
	repo := repomgr.Get(repoID)
	if repo == nil {
		err := fmt.Errorf("failed to get repo %s.\n", repoID)
		return err
	}

	headCommit, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		err := fmt.Errorf("failed to get head commit for repo %s", repo.ID)
		return err
	}

	relativeDirCan := getCanonPath(newDirPath)

	subFolders := strings.Split(relativeDirCan, "/")

	for _, name := range subFolders {
		if name == "" {
			continue
		}
		if shouldIgnoreFile(name) {
			err := fmt.Errorf("[post dir] invalid dir name %s.\n", name)
			return err
		}
	}

	var rootID string
	var parentDirCan, absPath string
	var uncreDirList []string
	if parentDir == "/" || parentDir == "\\" {
		parentDirCan = "/"
		absPath = filepath.Join(parentDirCan, relativeDirCan)
	} else {
		parentDirCan = getCanonPath(parentDir)
		absPath = filepath.Join(parentDirCan, relativeDirCan)
	}

	n := len(subFolders) - 1
	for i := n; i >= 0; i-- {
		if subFolders[i] == "" {
			continue
		}

		totalPathLen := len(absPath)
		subFolderLen := len(subFolders[i]) + 1
		absPath = absPath[:totalPathLen-subFolderLen]
		exist, _ := checkFileExists(repo.StoreID, headCommit.RootID, absPath, subFolders[i])
		if exist {
			absPath = filepath.Join(absPath, subFolders[i])
			break
		} else {
			uncreDirList = append([]string{subFolders[i]}, uncreDirList...)
		}
	}

	if uncreDirList != nil {
		newRootID := headCommit.RootID
		for _, uncreDir := range uncreDirList {
			dent := new(fsmgr.SeafDirent)
			dent.Name = uncreDir
			dent.ID = "0000000000000000000000000000000000000000"
			dent.Mtime = time.Now().Unix()
			dent.Mode = (syscall.S_IFDIR | 0644)
			rootID, _ = doPostFile(repo, newRootID, absPath, dent)
			if rootID == "" {
				err := fmt.Errorf("[put dir] failed to put dir.\n")
				return err
			}

			absPath = filepath.Join(absPath, uncreDir)
			newRootID = rootID
		}

		buf := fmt.Sprintf("Added directory \"%s\"", relativeDirCan)
		_, err = genNewCommit(repo, headCommit, rootID, user, buf)
		if err != nil {
			err := fmt.Errorf("failed to generate new commit: %v.\n", err)
			return err
		}

		go mergeVirtualRepo(repo.ID, "")
	}

	return nil
}

func doPostFile(repo *repomgr.Repo, rootID, parentDir string, dent *fsmgr.SeafDirent) (string, error) {
	return doPostFileReplace(repo, rootID, parentDir, 0, dent)
}
func doPostFileReplace(repo *repomgr.Repo, rootID, parentDir string, replace int, dent *fsmgr.SeafDirent) (string, error) {
	if strings.Index(parentDir, "/") == 0 {
		parentDir = parentDir[1:]
	}

	return postFileRecursive(repo, rootID, parentDir, replace, dent)
}

func postFileRecursive(repo *repomgr.Repo, dirID, toPath string, replace int, newDent *fsmgr.SeafDirent) (string, error) {
	olddir, err := fsmgr.GetSeafdir(repo.StoreID, dirID)
	if err != nil {
		err := fmt.Errorf("failed to get dir.\n")
		return "", err
	}
	sort.Sort(Dirents(olddir.Entries))

	var ret string
	if toPath == "" {
		var newEntries []fsmgr.SeafDirent
		if replace != 0 && fileNameExists(olddir.Entries, newDent.Name) {
			for _, dent := range olddir.Entries {
				if dent.Name == newDent.Name {
					newEntries = append(newEntries, *newDent)
				} else {
					newEntries = append(newEntries, dent)
				}
			}

			newdir := new(fsmgr.SeafDir)
			newdir.Version = 1
			newdir.Entries = newEntries
			jsonstr, err := json.Marshal(newdir)
			if err != nil {
				err := fmt.Errorf("failed to convert seafdir to json.\n")
				return "", err
			}
			checksum := sha1.Sum(jsonstr)
			id := hex.EncodeToString(checksum[:])
			err = fsmgr.SaveSeafdir(repo.StoreID, id, newdir)
			if err != nil {
				err := fmt.Errorf("failed to save seafdir %s/%s.\n", repo.ID, id)
				return "", err
			}
			return id, nil
		}

		uniqueName := genUniqueName(newDent.Name, olddir.Entries)
		if uniqueName == "" {
			err := fmt.Errorf("failed to generate unique name for %s.\n", newDent.Name)
			return "", err
		}
		dentDup := new(fsmgr.SeafDirent)
		dentDup.ID = newDent.ID
		dentDup.Mode = newDent.Mode
		dentDup.Mtime = newDent.Mtime
		dentDup.Name = uniqueName
		dentDup.Modifier = newDent.Modifier
		dentDup.Size = newDent.Size

		newEntries = make([]fsmgr.SeafDirent, len(olddir.Entries))
		copy(newEntries, olddir.Entries)
		newEntries = append(newEntries, *dentDup)
		sort.Sort(Dirents(newEntries))
		newdir := new(fsmgr.SeafDir)
		newdir.Version = 1
		newdir.Entries = newEntries
		jsonstr, err := json.Marshal(newdir)
		if err != nil {
			err := fmt.Errorf("failed to convert seafdir to json.\n")
			return "", err
		}
		checksum := sha1.Sum(jsonstr)
		id := hex.EncodeToString(checksum[:])
		err = fsmgr.SaveSeafdir(repo.StoreID, id, newdir)
		if err != nil {
			err := fmt.Errorf("failed to save seafdir %s/%s.\n", repo.ID, id)
			return "", err
		}

		return id, nil
	}

	var remain string
	if slash := strings.Index(toPath, "/"); slash >= 0 {
		remain = toPath[slash+1:]
	}

	entries := olddir.Entries
	for i, dent := range entries {
		if dent.Name == toPath {
			continue
		}

		id, err := postFileRecursive(repo, dent.ID, remain, replace, newDent)
		if err != nil {
			err := fmt.Errorf("failed to put dirent %s: %v.\n", dent.Name, err)
			return "", err
		}
		ret = id
		if id != "" {
			entries[i].ID = id
			entries[i].Mtime = time.Now().Unix()
		}
		break
	}

	if ret != "" {
		newDir := new(fsmgr.SeafDir)
		newDir.Version = 1
		newDir.Entries = olddir.Entries
		jsonstr, err := json.Marshal(newDir)
		if err != nil {
			err := fmt.Errorf("failed to convert seafdir to json.\n")
			return "", err
		}
		checkSum := sha1.Sum(jsonstr)
		id := hex.EncodeToString(checkSum[:])
		err = fsmgr.SaveSeafdir(repo.StoreID, id, newDir)
		if err != nil {
			err := fmt.Errorf("failed to save seafdir %s/%s.\n", repo.ID, id)
			return "", err
		}
		ret = id
	}

	return ret, nil
}
func fileNameExists(entries []fsmgr.SeafDirent, fileName string) bool {
	for _, de := range entries {
		if de.Name == fileName {
			return true
		}
	}

	return false
}

func checkFileExists(storeID, rootID, parentDir, fileName string) (bool, error) {
	dir, err := fsmgr.GetSeafdirByPath(storeID, rootID, parentDir)
	if err != nil {
		err := fmt.Errorf("parent_dir %s doesn't exist in repo %s.\n", parentDir, storeID)
		return false, err
	}

	var ret bool
	entries := dir.Entries
	for _, de := range entries {
		if de.Name == fileName {
			ret = true
			break
		}
	}

	return ret, nil
}

func checkTmpFileList(rsp http.ResponseWriter, fileNames []string) *appError {
	var totalSize int64
	for _, tmpFile := range fileNames {
		fileInfo, err := os.Stat(tmpFile)
		if err != nil {
			msg := "Internal error.\n"
			errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
			errReply.Error = fmt.Errorf("[upload] Failed to stat temp file %s.\n", tmpFile)
			return errReply
		}
		totalSize += fileInfo.Size()
	}

	if options.maxUploadSize > 0 && uint64(totalSize) > options.maxUploadSize {
		msg := "File size is too large.\n"
		errReply := sendErrorReply(rsp, msg, 442)
		return errReply
	}

	return nil
}

func parseFormValue(r *http.Request) (map[string]string, error) {
	formValue := make(map[string]string)

	formFiles := r.MultipartForm.File
	for name, fileHeaders := range formFiles {
		if name != "parent_dir" && name != "relative_path" && name != "replace" {
			continue
		}
		if len(fileHeaders) > 1 {
			err := fmt.Errorf("wrong multipart form data.\n")
			return nil, err
		}
		for _, handler := range fileHeaders {
			file, err := handler.Open()
			if err != nil {
				err := fmt.Errorf("failed to open file for read: %v.\n", err)
				return nil, err
			}
			defer file.Close()

			var buf bytes.Buffer
			_, err = buf.ReadFrom(file)
			if err != nil {
				err := fmt.Errorf("failed to read file: %v.\n", err)
				return nil, err
			}
			formValue[name] = buf.String()
		}
	}
	return formValue, nil
}

func writeBlockDataToTmpFile(r *http.Request, fsm *recvData, formFiles map[string][]*multipart.FileHeader,
	repoID, parentDir string) error {
	httpTempDir := filepath.Join(absDataDir, "httptemp")

	for name, fileHeaders := range formFiles {
		if name != "file" {
			continue
		}
		if fsm.rstart < 0 {
			for _, handler := range fileHeaders {
				file, err := handler.Open()
				if err != nil {
					err := fmt.Errorf("failed to open file for read: %v.\n", err)
					return err
				}
				defer file.Close()

				fileName := filepath.Base(handler.Filename)
				tmpFile, err := ioutil.TempFile(httpTempDir, fileName)
				if err != nil {
					err := fmt.Errorf("failed to create temp file: %v.\n", err)
					return err
				}

				io.Copy(tmpFile, file)

				fsm.fileNames = append(fsm.fileNames, fileName)
				fsm.files = append(fsm.files, tmpFile.Name())

			}

			return nil
		}

		disposition := r.Header.Get("Content-Disposition")
		if disposition == "" {
			err := fmt.Errorf("missing content disposition.\n")
			return err
		}

		for _, handler := range fileHeaders {
			file, err := handler.Open()
			if err != nil {
				err := fmt.Errorf("failed to open file for read: %v.\n", err)
				return err
			}
			defer file.Close()

			var f *os.File
			filename := handler.Filename
			filePath := filepath.Join("/", parentDir, filename)
			tmpFile, err := repomgr.GetUploadTmpFile(repoID, filePath)
			if err != nil || tmpFile == "" {
				tmpDir := filepath.Join(httpTempDir, "cluster-shared")
				f, err = ioutil.TempFile(tmpDir, filename)
				if err != nil {
					return err
				}
				repomgr.AddUploadTmpFile(repoID, filePath, f.Name())
				tmpFile = f.Name()
			} else {
				f, err = os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE, 0666)
				if err != nil {
					return err
				}
			}

			if fsm.rend == fsm.fsize-1 {
				fileName := filepath.Base(filename)
				fsm.fileNames = append(fsm.fileNames, fileName)
				fsm.files = append(fsm.files, tmpFile)
			}

			f.Seek(fsm.rstart, 0)
			io.Copy(f, file)
			f.Close()
		}

	}

	return nil
}

func checkParentDir(rsp http.ResponseWriter, repoID string, parentDir string) *appError {
	repo := repomgr.Get(repoID)
	if repo == nil {
		msg := "Failed to get repo.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
		errReply.Error = fmt.Errorf("Failed to get repo %s", repoID)
		return errReply
	}

	commit, err := commitmgr.Load(repoID, repo.HeadCommitID)
	if err != nil {
		msg := "Failed to get head commit.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusInternalServerError)
		errReply.Error = fmt.Errorf("Failed to get head commit for repo %s", repoID)
		return errReply
	}

	canonPath := getCanonPath(parentDir)

	_, err = fsmgr.GetSeafdirByPath(repo.StoreID, commit.RootID, canonPath)
	if err != nil {
		msg := "Parent dir doesn't exist.\n"
		errReply := sendErrorReply(rsp, msg, http.StatusBadRequest)
		return errReply
	}

	return nil
}

func isParentMatched(uploadDir, parentDir string) bool {
	uploadCanon := filepath.Join("/", uploadDir)
	parentCanon := filepath.Join("/", parentDir)
	if uploadCanon != parentCanon {
		return false
	}

	return true
}

func parseContentRange(ranges string, fsm *recvData) bool {
	start := strings.Index(ranges, "bytes")
	end := strings.Index(ranges, "-")
	slash := strings.Index(ranges, "/")

	if start < 0 || end < 0 || slash < 0 {
		return false
	}

	startStr := strings.TrimLeft(ranges[start+len("bytes"):end], " ")
	firstByte, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return false
	}

	lastByte, err := strconv.ParseInt(ranges[end+1:slash], 10, 64)
	if err != nil {
		return false
	}

	fileSize, err := strconv.ParseInt(ranges[slash+1:], 10, 64)
	if err != nil {
		return false
	}

	if firstByte > lastByte || lastByte >= fileSize {
		return false
	}

	fsm.rstart = firstByte
	fsm.rend = lastByte
	fsm.fsize = fileSize

	return true
}

type webaccessInfo struct {
	repoID string
	objID  string
	op     string
	user   string
}

func parseWebaccessInfo(rsp http.ResponseWriter, token string) (*webaccessInfo, *appError) {
	webaccess, err := rpcclient.Call("seafile_web_query_access_token", token)
	if err != nil {
		err := fmt.Errorf("failed to get web access token: %v.\n", err)
		return nil, &appError{err, "", http.StatusInternalServerError}
	}
	if webaccess == nil {
		msg := "Bad access token"
		return nil, &appError{err, msg, http.StatusBadRequest}
	}

	webaccessMap, ok := webaccess.(map[string]interface{})
	if !ok {
		return nil, &appError{nil, "", http.StatusInternalServerError}
	}

	accessInfo := new(webaccessInfo)
	repoID, ok := webaccessMap["repo-id"].(string)
	if !ok {
		return nil, &appError{nil, "", http.StatusInternalServerError}
	}
	accessInfo.repoID = repoID

	id, ok := webaccessMap["obj-id"].(string)
	if !ok {
		return nil, &appError{nil, "", http.StatusInternalServerError}
	}
	accessInfo.objID = id

	op, ok := webaccessMap["op"].(string)
	if !ok {
		return nil, &appError{nil, "", http.StatusInternalServerError}
	}
	accessInfo.op = op

	user, ok := webaccessMap["username"].(string)
	if !ok {
		return nil, &appError{nil, "", http.StatusInternalServerError}
	}
	accessInfo.user = user

	return accessInfo, nil
}