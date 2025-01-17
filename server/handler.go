package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

type resultCode struct {
	Ok      bool   `json:"ok"`
	Message string `json:"message"`
}

func respResult(w http.ResponseWriter, message string) {
	ret := &resultCode{
		Message: message,
	}
	if message == "" {
		ret.Ok = true
	}
	if err := json.NewEncoder(w).Encode(ret); err != nil {
		logger.Errorf(err.Error())
	}
}

/***********************************  拉取路径下的所有文件  ****************************************************/

type fileListResp struct {
	Ok            bool        `json:"ok"`
	Total         int         `json:"total"`
	Count         int         `json:"count"`
	Items         interface{} `json:"items"`
	DiskUsed      uint64      `json:"disk_used"`
	DiskTotal     uint64      `json:"disk_total"`
	DiskUsedP     float64     `json:"disk_used_p"`
	PathSeparator string      `json:"pathSeparator"`
}

type item struct {
	Filename string `json:"filename"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Date     string `json:"date"`
}

func respFileList(w http.ResponseWriter, ok bool, count int, data interface{}) {
	ret := &fileListResp{
		Ok: ok,
	}
	if ok {
		diskTotal, diskUsed, diskUsedP := diskUsed()
		ret.Count = count
		ret.Items = data
		ret.DiskUsed = diskUsed
		ret.DiskTotal = diskTotal
		ret.DiskUsedP = diskUsedP
		ret.PathSeparator = string(os.PathSeparator)
	}
	if err := json.NewEncoder(w).Encode(ret); err != nil {
		logger.Errorf(err.Error())
	}
}

/*
* 获取目录下文件， 正在上传的文件不显示。
* path -> 获取文件路径
 */
func fileList(w http.ResponseWriter, msg interface{}) {
	req := msg.(url.Values)
	filePath := req.Get("path")
	logger.Debug("fileList", filePath)

	filePtr.mtx.RLock()
	defer filePtr.mtx.RUnlock()
	info, err := filePtr.findPath(filePath, false)
	if err != nil {
		logger.Error(err)
		respFileList(w, false, 0, nil)
		return
	}

	data := map[string]item{}
	for _, info := range info.FileInfos {
		// 正在上传中的文件不同步
		if info.IsDir || info.FileOk {
			data[info.Name] = item{
				Filename: info.Name,
				IsDir:    info.IsDir,
				Size:     info.FileSize,
				Date:     info.FileDate,
			}
		}
	}
	respFileList(w, true, len(data), data)
}

/***********************************  删除路径下的文件  ****************************************************/

/*
* 删除文件，文件夹。
* path -> 文件路径
* filename -> 文件名，文件夹名。
 */
func fileDelete(w http.ResponseWriter, msg interface{}) {
	req := msg.(url.Values)
	filePath := req.Get("path")
	filename := req.Get("filename")
	logger.Debug("fileDelete", filePath, filename)

	if filename == "" {
		respResult(w, "请求参数错误!")
		return
	}

	filePtr.mtx.Lock()
	defer filePtr.mtx.Unlock()
	info, err := filePtr.findPath(filePath, false)
	if err != nil {
		logger.Error(err)
		respResult(w, "文件不存在!")
		return
	}

	if err = filePtr.remove(info, filename); err != nil {
		logger.Error(err)
		respResult(w, "文件不存在!")
		return

	}

	respResult(w, "")
}

/***********************************  新建文件夹  ****************************************************/

/*
* 新建文件夹。
* path -> 文件夹路径
 */
func fileMkdir(w http.ResponseWriter, msg interface{}) {
	req := msg.(url.Values)
	filePath := req.Get("path")
	logger.Debug("fileMkdir", filePath)

	if filePath == "" {
		respResult(w, "请求参数错误!")
		return
	}

	filePtr.mtx.Lock()
	defer filePtr.mtx.Unlock()
	_, err := filePtr.findPath(filePath, false)
	if err != nil {
		logger.Error(err)
		respResult(w, "文件夹名错误，可能与文件名相同")
		return
	}
	respResult(w, "")
}

/***********************************  文件上传前的检查  ****************************************************/

type fileCheckReq struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
	MD5      string `json:"md5"`
	Total    int    `json:"total"`
	Size     int64  `json:"size"`
}

type fileCheckResp struct {
	Ok      bool              `json:"ok"`
	Message string            `json:"message"`
	Need    bool              `json:"need"` // 需要上传,不需要上传
	Upload  map[string]string `json:"upload"`
}

func respFileCheck(w http.ResponseWriter, err error, need bool, up map[string]string) {
	ret := &fileCheckResp{}
	if err != nil {
		ret.Ok = false
		ret.Message = err.Error()
	} else {
		ret.Ok = true
		ret.Need = need
		ret.Upload = up
	}
	if err := json.NewEncoder(w).Encode(ret); err != nil {
		logger.Errorf(err.Error())
	}
}

func fileCheck(w http.ResponseWriter, msg interface{}) {
	req := msg.(*fileCheckReq)
	logger.Info("fileCheck", req)

	if req.Path == "" || req.Filename == "" || req.MD5 == "" || req.Size == 0 || req.Total == 0 {
		respFileCheck(w, fmt.Errorf("参数请求错误！"), false, nil)
		return
	}

	filePtr.mtx.Lock()
	defer filePtr.mtx.Unlock()
	info, err := filePtr.findPath(req.Path, true)
	if err != nil {
		logger.Error(err)
		respFileCheck(w, err, false, nil)
		return
	}
	up := &upload{
		Size:     req.Size,
		MD5:      req.MD5,
		SliceCnt: req.Total,
		UpSlice:  map[string]string{},
	}

	absPath := filepath.Join(info.AbsPath, req.Filename)
	file, ok := info.FileInfos[req.Filename]
	if !ok {
		newInfo := &fileInfo{
			Path:    filepath.Join(info.Path, info.Name),
			Name:    req.Filename,
			AbsPath: absPath,
			FileMD5: req.MD5,
		}

		files, md5Ok := filePtr.MD5Files[req.MD5]
		if md5Ok {
			// 已存在md5文件
			if config.SaveFileMultiple {
				// 真实保存,拷贝文件
				if _, err := CopyFile(files.Ptr[0], absPath); err != nil {
					logger.Error(err)
					respFileCheck(w, err, false, nil)
					return
				}
			}
			newInfo.FileOk = true
			newInfo.FileSize = files.Size
			newInfo.FileDate = nowFormat()
			info.FileInfos[newInfo.Name] = newInfo
			filePtr.addMD5File(newInfo.FileMD5, newInfo)
			respFileCheck(w, nil, false, nil)

		} else {
			// 不存在md5文件，新建
			newInfo.Upload = up
			info.FileInfos[newInfo.Name] = newInfo
			respFileCheck(w, nil, true, nil)
		}
	} else {
		if file.IsDir {
			respFileCheck(w, fmt.Errorf("已存在同名文件夹"), false, nil)
			return
		}
		if file.FileMD5 != req.MD5 {
			// 原文件已经改变，需要上传
			if file.Upload == nil {
				file.Upload = up
				respFileCheck(w, nil, true, nil)
			} else {
				if file.Upload.MD5 == req.MD5 {
					// 新文件已经上传了一部分
					file.mergeUpload()
					respFileCheck(w, nil, true, file.Upload.UpSlice)
				} else {
					// 新文件没有上传完，但又上传不同md5文件
					file.clearUpload()
					file.Upload = up
					respFileCheck(w, nil, true, nil)
				}
			}
		} else {
			if file.Upload == nil {
				// 已经上传完成,不需要上传
				respFileCheck(w, nil, false, nil)
			} else {
				if file.Upload.MD5 == req.MD5 {
					// 新文件已经上传了一部分
					respFileCheck(w, nil, true, file.Upload.UpSlice)
				} else {
					// 新文件没有上传完，但又上传不同md5文件
					file.clearUpload()
					file.Upload = up
					respFileCheck(w, nil, true, nil)
				}
			}
		}
	}
}

/***********************************  文件上传  ****************************************************/

/*
 * 文件上传，创建路径。
 * path -> 文件路径
 * filename -> 文件名。
 * current -> 当前文件分片。
 * md5 -> 文件md5值。比对文件变化。
 * file -> 文件分片。
 */

func fileUpload(w http.ResponseWriter, r *http.Request) {
	filePath := r.FormValue("path")
	filename := r.FormValue("filename")
	md5 := r.FormValue("md5")
	current := r.FormValue("current")

	logger.Info("fileUpload", r.RemoteAddr, filePath, filename, md5, current)

	if filePath == "" || filename == "" || md5 == "" || current == "" {
		respResult(w, "参数请求错误！")
		return
	}

	filePtr.mtx.Lock()
	defer filePtr.mtx.Unlock()
	info, err := filePtr.findPath(filePath, false)
	if err != nil {
		logger.Error(err)
		respResult(w, "路径不存在")
		return
	}

	file, ok := info.FileInfos[filename]
	if !ok || file.Upload == nil || file.Upload.MD5 != md5 {
		respResult(w, "上传流程错误，check！")
		return
	}

	_, ok = file.Upload.UpSlice[current]
	if ok {
		// 当前分片已经上传
		respResult(w, "")
		return
	}

	gFile, _, err := r.FormFile("file")
	if err != nil {
		logger.Error(err)
		respResult(w, err.Error())
		return
	}
	defer gFile.Close()

	partFilename := makeFilePart(file.AbsPath, current)
	if _, err = WriteFile(partFilename, gFile); err != nil {
		logger.Debug(err.Error())
		respResult(w, err.Error())
		return
	}

	file.Upload.UpSlice[current] = ""
	file.mergeUpload()

	respResult(w, "")
}

/*
* 文件下载
* path -> 文件路径
* filename -> 文件名。
 */
func fileDownload(w http.ResponseWriter, msg interface{}) {
	req := msg.(url.Values)
	filePath := req.Get("path")
	filename := req.Get("filename")
	logger.Debug("fileDownload", filePath, filename)

	filePtr.mtx.Lock()
	defer filePtr.mtx.Unlock()
	info, err := filePtr.findPath(filePath, false)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Bad request")
		return
	}

	file, ok := info.FileInfos[filename]
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Bad request")
		return
	}

	absPath := file.AbsPath
	if !config.SaveFileMultiple {
		// 虚拟保存，修正到真实文件路径
		md5File_, ok := filePtr.MD5Files[file.FileMD5]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Bad request")
			return
		}
		absPath = md5File_.File
	}

	//打开文件
	f, err := os.Open(absPath)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Bad request")
		return
	}
	//结束后关闭文件
	defer f.Close()

	//设置响应的header头
	w.Header().Add("Content-type", "application/octet-stream")
	w.Header().Add("content-disposition", "attachment; filename=\""+filename+"\"")
	//将文件写至responseBody
	_, err = io.Copy(w, f)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Bad request")
	}
}

/*
 * 文件动作
 * action -> 行为 move copy
 * filename -> 文件名。
 * src -> 文件原路径
 * dest -> 文件新路径
 */
func fileAction(w http.ResponseWriter, msg interface{}) {
	req := msg.(url.Values)
	action := req.Get("action")
	filename := req.Get("filename")
	src := req.Get("src")
	dest := req.Get("dest")
	logger.Debug("fileAction", action, filename, src, dest)

	filePtr.mtx.Lock()
	defer filePtr.mtx.Unlock()
	srcInfo, err := filePtr.findPath(src, false)
	if err != nil {
		respResult(w, "request argument failed, "+err.Error())
		return
	}
	destInfo, err := filePtr.findPath(dest, true)
	if err != nil {
		respResult(w, "request argument failed, "+err.Error())
		return
	}

	srcFile, ok := srcInfo.FileInfos[filename]
	if !ok {
		respResult(w, "request argument failed")
		return
	}

	newInfo := &fileInfo{
		Path:     filepath.Join(destInfo.Path, destInfo.Name),
		Name:     filename,
		AbsPath:  filepath.Join(destInfo.AbsPath, filename),
		FileMD5:  srcFile.FileMD5,
		FileOk:   true,
		FileSize: srcFile.FileSize,
		FileDate: nowFormat(),
	}

	files, _ := filePtr.MD5Files[srcFile.FileMD5]
	if config.SaveFileMultiple {
		// 真实保存，拷贝文件到真实文件路径
		if _, err := CopyFile(files.Ptr[0], newInfo.AbsPath); err != nil {
			logger.Error(err)
			respResult(w, "copy file failed, "+err.Error())
			return
		}
	}
	destInfo.FileInfos[newInfo.Name] = newInfo
	filePtr.addMD5File(newInfo.FileMD5, newInfo)

	switch action {
	case "move":
		// 移除原文件
		filePtr.remove(srcInfo, filename)
	case "copy":
	default:
		respResult(w, "request argument failed")
	}

	respResult(w, "")
}
