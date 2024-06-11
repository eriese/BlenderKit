/*##### BEGIN GPL LICENSE BLOCK #####

  This program is free software; you can redistribute it and/or
  modify it under the terms of the GNU General Public License
  as published by the Free Software Foundation; either version 2
  of the License, or (at your option) any later version.

  This program is distributed in the hope that it will be useful,
  but WITHOUT ANY WARRANTY; without even the implied warranty of
  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
  GNU General Public License for more details.

  You should have received a copy of the GNU General Public License
  along with this program; if not, write to the Free Software Foundation,
  Inc., 51 Franklin Street, Fifth Floor, Boston, MA 02110-1301, USA.

##### END GPL LICENSE BLOCK #####*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gookit/color"
)

const (
	ReportTimeout   = 3 * time.Minute
	OAUTH_CLIENT_ID = "IdFRwa3SGA8eMpzhRVFMg5Ts8sPK93xBjif93x0F"

	// PATHS
	server_default     = "https://www.blenderkit.com" // default address to production blenderkit server
	gravatar_dirname   = "bkit_g"                     // directory in safeTempDir() for gravatar images
	cleanfile_path     = "blendfiles/cleaned.blend"   // relative path to clean blend file in add-on directory
	upload_script_path = "upload_bg.py"               // relative path to upload script in add-on directory

	// EMOJIS
	EmoOK            = "✅"
	EmoCancel        = "⛔"
	EmoWarning       = "⚠️ " // Needs space at the end for proper alignment, not sure why.
	EmoInfo          = "ℹ️ "
	EmoError         = "❌"
	EmoNetwork       = "📡"
	EmoNewConnection = "🤝"
	EmoDisconnecting = "👐"
	EmoSecure        = "🔒"
	EmoInsecure      = "🧨"
	EmoUpload        = "⬆️ "
	EmoDownload      = "⬇️ "
	EmoIdentity      = "🆔"
	EmoUpdate        = "🔜"
)

var (
	ClientVersion = "0.0.0" // Version of this BlenderKit-client binary, set from file client/VERSION with -ldflags during build in dev.py
	SystemID      *string   // Unique ID of the current system (string of 15 integers)
	Port          *string
	Server        *string

	OAuth2Sessions    map[string]OAuth2VerificationData // Map of OAuth2 sessions, key is the state string
	OAuth2SessionsMux sync.Mutex

	lastReportAccess    time.Time
	lastReportAccessMux sync.Mutex

	ActiveAppsMux sync.Mutex
	ActiveApps    []int

	Tasks                map[int]map[string]*Task
	TasksMux             sync.Mutex
	AddTaskCh            chan *Task
	TaskProgressUpdateCh chan *TaskProgressUpdate
	TaskMessageCh        chan *TaskMessageUpdate
	TaskFinishCh         chan *TaskFinish
	TaskErrorCh          chan *TaskError
	TaskCancelCh         chan *TaskCancel

	ClientAPI, ClientDownloads, ClientUploads, ClientSmallThumbs, ClientBigThumbs *http.Client

	BKLog   *log.Logger
	ChanLog *log.Logger
)

func init() {
	SystemID = getSystemID()
	OAuth2Sessions = make(map[string]OAuth2VerificationData)
	Tasks = make(map[int]map[string]*Task)
	AddTaskCh = make(chan *Task, 1000)
	TaskProgressUpdateCh = make(chan *TaskProgressUpdate, 1000)
	TaskMessageCh = make(chan *TaskMessageUpdate, 1000)
	TaskFinishCh = make(chan *TaskFinish, 1000)
	TaskCancelCh = make(chan *TaskCancel, 1000)
	TaskErrorCh = make(chan *TaskError, 1000)

	BKLog = log.New(os.Stdout, "⬡  ", log.LstdFlags)   // Hexagon like BlenderKit logo
	ChanLog = log.New(os.Stdout, "<- ", log.LstdFlags) // Same symbols as channel in Go
}

// Endless loop to handle channels
func handleChannels() {
	for {
		select {
		case task := <-AddTaskCh:
			TasksMux.Lock()
			if Tasks[task.AppID] == nil {
				BKLog.Printf("%s Unexpected: AppID %d not in Tasks! Add-on should first make report requst, then shedule tasks, fix this!", EmoWarning, task.AppID)
				data := MinimalTaskData{AppID: task.AppID}
				SubscribeNewApp(data)
			}
			Tasks[task.AppID][task.TaskID] = task
			TasksMux.Unlock()
			// Task can be created directly with status "finished" or "error"
			if task.Status == "error" {
				ChanLog.Printf("%s %s (%s): %v\n", EmoError, task.TaskType, task.TaskID, task.Error)
			}
			if task.Status == "finished" {
				ChanLog.Printf("%s %s (%s)\n", EmoOK, task.TaskType, task.TaskID)
			}
		case u := <-TaskProgressUpdateCh:
			if u.Message != "" {
				ChanLog.Printf("%s progress on task %s (%d) - %d%%: %s\n", EmoUpdate, u.TaskID, u.AppID, u.Progress, u.Message)
			} else {
				ChanLog.Printf("%s progress on task %s (%d) - %d%%\n", EmoUpdate, u.TaskID, u.AppID, u.Progress)
			}
			TasksMux.Lock()
			task := Tasks[u.AppID][u.TaskID]
			task.Progress = u.Progress
			if u.Message != "" {
				task.Message = u.Message
			}
			if u.MessageDetailed != "" {
				task.MessageDetailed = u.MessageDetailed
			}
			TasksMux.Unlock()
		case m := <-TaskMessageCh:
			TasksMux.Lock()
			task := Tasks[m.AppID][m.TaskID]
			task.Message = m.Message
			if m.MessageDetailed != "" {
				task.MessageDetailed = m.MessageDetailed
			}
			TasksMux.Unlock()
			ChanLog.Printf("%s %s (%s): %s\n", EmoInfo, task.TaskType, task.TaskID, m.Message)
		case f := <-TaskFinishCh:
			TasksMux.Lock()
			task := Tasks[f.AppID][f.TaskID]
			task.Status = "finished"
			task.Result = f.Result
			if f.Message != "" {
				task.Message = f.Message
			}
			if f.MessageDetailed != "" {
				task.MessageDetailed = f.MessageDetailed
			}
			TasksMux.Unlock()
			ChanLog.Printf("%s %s (%s)\n", EmoOK, task.TaskType, task.TaskID)
		case e := <-TaskErrorCh:
			TasksMux.Lock()
			task := Tasks[e.AppID][e.TaskID]
			if task.Status == "cancelled" {
				delete(Tasks[e.AppID], e.TaskID)
				TasksMux.Unlock()
				ChanLog.Printf("%s ignored on %s (%s): %s, task in cancelled status\n", EmoCancel, task.TaskType, task.TaskID, e.Error)
				continue
			}
			task.Message = fmt.Sprintf("%v", e.Error)
			if e.Result != nil {
				task.Result = e.Result
			}
			if e.MessageDetailed != "" {
				task.MessageDetailed = e.MessageDetailed
			}
			task.Status = "error"
			TasksMux.Unlock()
			ChanLog.Printf("%s in %s (%s): %v\n", EmoError, task.TaskType, task.TaskID, e.Error)
		case k := <-TaskCancelCh:
			TasksMux.Lock()
			task := Tasks[k.AppID][k.TaskID]
			task.Status = "cancelled"
			task.Cancel()
			TasksMux.Unlock()
			ChanLog.Printf("%s %s (%s), reason: %s\n", EmoCancel, task.TaskType, task.TaskID, k.Reason)
		}
	}
}

func main() {
	Port = flag.String("port", "62485", "port to listen on")
	Server = flag.String("server", server_default, "server to connect to")
	ssl_context := flag.String("ssl_context", "DEFAULT", "SSL context to use") // possible values: "DEFAULT", "PRECONFIGURED", "DISABLED"
	proxy_which := flag.String("proxy_which", "SYSTEM", "proxy to use")        // possible values: "SYSTEM", "NONE", "CUSTOM"
	proxy_address := flag.String("proxy_address", "", "proxy address")
	trusted_ca_certs := flag.String("trusted_ca_certs", "", "trusted CA certificates")
	addon_version := flag.String("version", "", "addon version")
	flag.Parse()
	fmt.Print("\n\n")
	BKLog.Printf("BlenderKit-Client v%s starting from add-on v%s\n   port=%s\n   server=%s\n   proxy_which=%s\n   proxy_address=%s\n   trusted_ca_certs=%s\n   ssl_context=%s",
		ClientVersion, *addon_version, *Port, *Server, *proxy_which, *proxy_address, *trusted_ca_certs, *ssl_context)

	CreateHTTPClients(*proxy_address, *proxy_which, *ssl_context, *trusted_ca_certs)
	go monitorReportAccess()
	go handleChannels()

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/report", reportHandler)
	mux.HandleFunc("/shutdown", shutdownHandler)
	mux.HandleFunc("/debug", DebugNetworkHandler)

	// LOGIN
	mux.HandleFunc("/consumer/exchange/", consumerExchangeHandler)
	mux.HandleFunc("/refresh_token", RefreshTokenHandler)
	mux.HandleFunc("/oauth2/verification_data", OAuth2VerificationDataHandler)
	mux.HandleFunc("/oauth2/logout", OAuth2LogoutHandler)

	// BLENDER SPECIFIC HANDLERS
	mux.HandleFunc("/blender/unsubscribe_addon", blenderUnsubscribeAddonHandler)
	mux.HandleFunc("/blender/cancel_download", CancelDownloadHandler)
	mux.HandleFunc("/blender/asset_download", assetDownloadHandler)
	mux.HandleFunc("/blender/asset_search", assetSearchHandler)
	mux.HandleFunc("/blender/asset_upload", assetUploadHandler)

	// API HANDLERS
	mux.HandleFunc("/profiles/download_gravatar_image", DownloadGravatarImageHandler)
	mux.HandleFunc("/profiles/get_user_profile", GetUserProfileHandler)

	mux.HandleFunc("/comments/get_comments", GetCommentsHandler)
	mux.HandleFunc("/comments/create_comment", CreateCommentHandler)
	mux.HandleFunc("/comments/feedback_comment", FeedbackCommentHandler)
	mux.HandleFunc("/comments/mark_comment_private", MarkCommentPrivateHandler)

	mux.HandleFunc("/notifications/mark_notification_read", MarkNotificationReadHandler)

	mux.HandleFunc("/ratings/get_bookmarks", GetBookmarksHandler)
	mux.HandleFunc("/ratings/get_rating", GetRatingHandler)
	mux.HandleFunc("/ratings/send_rating", SendRatingHandler)

	// WRAPPERS
	mux.HandleFunc("/wrappers/get_download_url", GetDownloadURLWrapper)
	mux.HandleFunc("/wrappers/complete_upload_file_blocking", CompleteUploadFileBlocking)
	mux.HandleFunc("/wrappers/blocking_file_download", BlockingFileDownloadHandler)
	mux.HandleFunc("/wrappers/blocking_request", BlockingRequestHandler)
	mux.HandleFunc("/wrappers/nonblocking_request", NonblockingRequestHandler)

	StartClient(mux)
}

// Start Client server on localhost, if this address cannot be used then it falls back to IPv4 127.0.0.1.
func StartClient(mux *http.ServeMux) {
	var addrs = []string{
		fmt.Sprintf("localhost:%s", *Port),
		fmt.Sprintf("127.0.0.1:%s", *Port),
	}
	for i, addr := range addrs {
		err := http.ListenAndServe(addr, mux)
		if err == nil {
			BKLog.Printf("%s Server finished %s\n", EmoOK, addr)
			return
		}

		var emo string
		if i < len(addrs)-1 {
			emo = EmoWarning
		} else {
			emo = EmoError
		}
		BKLog.Printf("%s Failed to start Client server on %s: %v\n", emo, addr, err)
	}
}

func monitorReportAccess() {
	for {
		time.Sleep(ReportTimeout)
		lastReportAccessMux.Lock()
		if time.Since(lastReportAccess) > ReportTimeout {
			BKLog.Printf("No /report access for %v minutes, shutting down.", ReportTimeout)
			os.Exit(0)
		}
		lastReportAccessMux.Unlock()
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	pid := os.Getpid()
	fmt.Fprintf(w, "%d", pid)
}

func shutdownHandler(w http.ResponseWriter, r *http.Request) {
	go delayedExit(0.1)
	w.WriteHeader(http.StatusOK)
}

func reportHandler(w http.ResponseWriter, r *http.Request) {
	lastReportAccessMux.Lock()
	lastReportAccess = time.Now()
	lastReportAccessMux.Unlock()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var data MinimalTaskData
	err = json.Unmarshal(body, &data)
	if err != nil {
		BKLog.Println("Error parsing ReportData:", err)
		http.Error(w, "Error parsing JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if data.AddonVersion == "" {
		msg := fmt.Sprintf("BlenderKit-Client running on port %s", *Port)
		BKLog.Printf("%v Add-on (probably v3.11 or less) requesting /report rejected.", EmoWarning)
		http.Error(w, msg, http.StatusForbidden) // 403
		return
	}

	TasksMux.Lock()
	if Tasks[data.AppID] == nil { // New add-on connected
		SubscribeNewApp(data)
	}

	taskID := uuid.New().String()
	reportTask := NewTask(nil, data.AppID, taskID, "client_status")
	reportTask.Finish("Client is running")

	toReport := make([]*Task, 0, len(Tasks[data.AppID]))
	toReport = append(toReport, reportTask)
	for _, task := range Tasks[data.AppID] {
		if task.AppID != data.AppID {
			continue
		}
		toReport = append(toReport, task)
		if task.Status == "finished" || task.Status == "error" {
			delete(Tasks[data.AppID], task.TaskID)
		}
	}
	TasksMux.Unlock()

	responseJSON, err := json.Marshal(toReport)
	if err != nil {
		http.Error(w, "Error converting to JSON: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(responseJSON)
}

// SubscribeNewApp adds new App into Tasks[AppID].
// This is called when new AppID appears - meeaning new add-on or other app wants to communicate with Client.
func SubscribeNewApp(data MinimalTaskData) {
	BKLog.Printf("%s New add-on connected: %d", EmoNewConnection, data.AppID)
	Tasks[data.AppID] = make(map[string]*Task)

	go FetchDisclaimer(data)
	go FetchCategories(data)
	if data.APIKey != "" {
		go FetchUnreadNotifications(data)
		go GetBookmarks(data)
		go GetUserProfile(data)
	}
}

func (t *Task) Finish(message string) {
	t.Status = "finished"
	t.Message = message
}
func NewTask(data interface{}, appID int, taskID, taskType string) *Task {
	if data == nil { // so it is not returned as None, but as empty dict{}
		data = make(map[string]interface{})
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Task{
		Data:            data,
		AppID:           appID,
		TaskID:          taskID,
		TaskType:        taskType,
		Message:         "",
		MessageDetailed: "",
		Progress:        0,
		Status:          "created",
		Result:          make(map[string]interface{}),
		Error:           nil,
		Ctx:             ctx,
		Cancel:          cancel,
	}
}

func blenderUnsubscribeAddonHandler(w http.ResponseWriter, r *http.Request) {
	var data ReportData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		http.Error(w, "Error parsing JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	BKLog.Printf("%s Add-on unsubscribed: %d", EmoDisconnecting, data.AppID)

	TasksMux.Lock()
	if Tasks[data.AppID] != nil {
		for _, task := range Tasks[data.AppID] {
			task.Cancel()
		}
		delete(Tasks, data.AppID)
	}
	TasksMux.Unlock()

	if len(Tasks) == 0 {
		BKLog.Printf("%s No add-ons left, shutting down...", EmoWarning)
		go delayedExit(0.1)
	}
	w.WriteHeader(http.StatusOK)
}

func delayedExit(t float64) {
	time.Sleep(time.Duration(t * float64(time.Second)))
	BKLog.Println("Bye!")
	os.Exit(0)
}

func assetSearchHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading search request body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var data SearchTaskData
	err = json.Unmarshal(body, &data)
	if err != nil {
		http.Error(w, "Error parsing JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	var rJSON map[string]interface{}
	err = json.Unmarshal(body, &rJSON)
	if err != nil {
		fmt.Println(">> Error parsing JSON:", err)
		http.Error(w, "Error parsing JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	taskID := uuid.New().String()
	go doAssetSearch(data, taskID)

	resData := map[string]string{"task_id": taskID}
	responseJSON, err := json.Marshal(resData)
	if err != nil {
		http.Error(w, "Error converting to JSON: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(responseJSON)
}

func doAssetSearch(data SearchTaskData, taskUUID string) {
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "search")

	req, err := http.NewRequest("GET", data.URLQuery, nil)
	if err != nil {
		err = fmt.Errorf("search - creating request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	req.Header = getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)

	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("search - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("search: %s, status (%s), query: %v", respString, resp.Status, data.URLQuery)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("search: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var searchResult SearchResults
	if err := json.NewDecoder(resp.Body).Decode(&searchResult); err != nil {
		err = fmt.Errorf("search - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{AppID: data.AppID, TaskID: taskUUID, Result: searchResult}
	go parseThumbnails(searchResult, data)
}

func parseThumbnails(searchResults SearchResults, data SearchTaskData) {
	var smallThumbsTasks, fullThumbsTasks []*Task
	blVer, _ := StringToBlenderVersion(data.BlenderVersion)

	for i, result := range searchResults.Results { // TODO: Should be a function parseThumbnail() to avaid nesting
		useWebp := false
		if result.WebpGeneratedTimestamp > 0 {
			useWebp = true
		}
		if blVer.Major < 3 || (blVer.Major == 3 && blVer.Minor < 4) {
			useWebp = false
		}

		var smallThumbURL, fullThumbURL string
		if useWebp {
			smallThumbURL = result.ThumbnailSmallURLWebp
			if result.AssetType == "hdr" {
				fullThumbURL = result.ThumbnailLargeURLNonsquaredWebp
			} else {
				fullThumbURL = result.ThumbnailMiddleURLWebp
			}
		} else {
			smallThumbURL = result.ThumbnailSmallURL
			if result.AssetType == "hdr" {
				fullThumbURL = result.ThumbnailLargeURLNonsquared
			} else {
				fullThumbURL = result.ThumbnailMiddleURL
			}
		}

		smallImgName, smallImgNameErr := ExtractFilenameFromURL(smallThumbURL)
		smallImgPath := filepath.Join(data.TempDir, smallImgName)
		smallTaskData := DownloadThumbnailData{
			AddonVersion:  data.AddonVersion,
			ThumbnailType: "small",
			ImagePath:     smallImgPath,
			ImageURL:      smallThumbURL,
			AssetBaseID:   result.AssetBaseID,
			Index:         i,
		}
		smallTaskUUID := uuid.New().String()
		smallTask := NewTask(smallTaskData, data.AppID, smallTaskUUID, "thumbnail_download")
		if smallImgNameErr != nil {
			smallTask.Error = fmt.Errorf("error extracting filename from URL: %v, for asset: %s ", smallImgNameErr, result.DisplayName)
		}
		smallThumbsTasks = append(smallThumbsTasks, smallTask)

		fullImgName, fullImgNameErr := ExtractFilenameFromURL(fullThumbURL)
		fullImgPath := filepath.Join(data.TempDir, fullImgName)
		fullTaskData := DownloadThumbnailData{
			AddonVersion:  data.AddonVersion,
			ThumbnailType: "full",
			ImagePath:     fullImgPath,
			ImageURL:      fullThumbURL,
			AssetBaseID:   result.AssetBaseID,
			Index:         i,
		}
		fullTaskUUID := uuid.New().String()
		fullTask := NewTask(fullTaskData, data.AppID, fullTaskUUID, "thumbnail_download")
		if fullImgNameErr != nil {
			fullTask.Error = fmt.Errorf("error extracting filename from URL: %v, for asset: %s", fullImgNameErr, result.DisplayName)
		}
		fullThumbsTasks = append(fullThumbsTasks, fullTask)
	}
	go downloadImageBatch(smallThumbsTasks, true)
	go downloadImageBatch(fullThumbsTasks, true)
}

func downloadImageBatch(tasks []*Task, block bool) {
	wg := new(sync.WaitGroup)
	for _, task := range tasks {
		wg.Add(1)
		go DownloadThumbnail(task, wg)
	}
	if block {
		wg.Wait()
	}
}

func DownloadThumbnail(t *Task, wg *sync.WaitGroup) {
	defer wg.Done()
	if t.Error != nil { // error from ExtractFilenameFromURL() in parseThumbnails()
		t.Status = "error"
		AddTaskCh <- t
		return
	}

	data, ok := t.Data.(DownloadThumbnailData)
	if !ok {
		t.Status = "error"
		t.Error = fmt.Errorf("invalid data type")
		AddTaskCh <- t
		return
	}

	if _, err := os.Stat(data.ImagePath); err == nil {
		t.Status = "finished"
		t.Message = "thumbnail on disk"
		AddTaskCh <- t
		return
	}

	req, err := http.NewRequest("GET", data.ImageURL, nil)
	if err != nil {
		t.Status = "error"
		t.Error = err
		AddTaskCh <- t
		return
	}

	headers := getHeaders("", *SystemID, data.AddonVersion, data.PlatformVersion)
	req.Header = headers
	resp, err := ClientBigThumbs.Do(req)
	if err != nil {
		t.Message = "Error performing request to download thumbnail"
		t.Status = "error"
		t.Error = err
		AddTaskCh <- t
		return
	}
	defer resp.Body.Close()
	//MARK: TODO check status code
	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		t.Message = fmt.Sprintf("search: %s, status (%s), url: %v", respString, resp.Status, data.ImageURL)
		t.Status = "error"
		AddTaskCh <- t
		return
	}

	// Open the file for writing
	file, err := os.Create(data.ImagePath)
	if err != nil {
		t.Message = "Error creating file for thumbnail"
		t.Status = "error"
		t.Error = err
		AddTaskCh <- t
		return
	}
	defer file.Close()

	// Copy the response body to the file
	if _, err := io.Copy(file, resp.Body); err != nil {
		t.Message = "Error copying thumbnail response body to file"
		t.Status = "error"
		t.Error = err
		AddTaskCh <- t
		return
	}

	t.Status = "finished"
	t.Message = "thumbnail downloaded"
	AddTaskCh <- t
}

// Fetch categories from the server: https://www.blenderkit.com/api/v1/categories/
// API documentation: https://www.blenderkit.com/api/v1/docs/#operation/categories_list
func FetchCategories(data MinimalTaskData) {
	url := *Server + "/api/v1/categories"
	taskUUID := uuid.New().String()
	task := NewTask(nil, data.AppID, taskUUID, "categories_update")
	AddTaskCh <- task

	headers := getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		err = fmt.Errorf("categories - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	req.Header = headers
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("categories - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("categories: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("categories: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData CategoriesData
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("categories - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	fix_category_counts(respData.Results)

	TaskFinishCh <- &TaskFinish{AppID: data.AppID, TaskID: taskUUID, Message: "Categories updated", Result: respData.Results}
}

// Fetch disclaimer from the server: https://www.blenderkit.com/api/v1/disclaimer/active/.
// API documentation:  https://www.blenderkit.com/api/v1/docs/#operation/disclaimer_active_list
func FetchDisclaimer(data MinimalTaskData) {
	url := *Server + "/api/v1/disclaimer/active/"
	taskUUID := uuid.New().String()
	task := NewTask(nil, data.AppID, taskUUID, "disclaimer")
	AddTaskCh <- task

	headers := getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		err = fmt.Errorf("disclaimer - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	req.Header = headers
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("disclaimer - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("disclaimer: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("disclaimer: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData DisclaimerData
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("disclaimer - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{AppID: data.AppID, TaskID: taskUUID, Message: "Disclaimer fetched", Result: respData}
}

// Fetch unread notifications from the server: https://www.blenderkit.com/api/v1/notifications/unread/.
// API documentation: https://www.blenderkit.com/api/v1/docs/#operation/notifications_unread_list
func FetchUnreadNotifications(data MinimalTaskData) {
	url := *Server + "/api/v1/notifications/unread/"
	taskUUID := uuid.New().String()
	task := NewTask(nil, data.AppID, taskUUID, "notifications")
	AddTaskCh <- task

	headers := getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		err = fmt.Errorf("notifications - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	req.Header = headers
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("notifications - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("notifications: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("notifications: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData NotificationData
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("notifications - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{AppID: data.AppID, TaskID: taskUUID, Message: "Notifications fetched", Result: respData}
}

func CancelDownloadHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var data CancelDownloadData
	err = json.Unmarshal(body, &data)
	if err != nil {
		http.Error(w, "Error parsing JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	TaskCancelCh <- &TaskCancel{
		AppID:  data.AppID,
		TaskID: data.TaskID,
		Reason: "cancelled by user",
	}
	w.WriteHeader(http.StatusOK)
}

// GetDownloadURLWrapper Handle get_download_url request. This serves as a wrapper around get_download_url so this can be called from addon.
// Returns the results directly so it is a blocking on add-on side (as add-on uses blocking Requests for this).
// It adds filename to the response, because BG scripts need it.
func GetDownloadURLWrapper(w http.ResponseWriter, r *http.Request) {
	data := DownloadData{}
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		http.Error(w, "Error parsing JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	canDownload, URL, err := GetDownloadURL(data)
	if err != nil {
		http.Error(w, "Error getting download URL: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fileName, err := ExtractFilenameFromURL(URL)
	if err != nil {
		http.Error(w, "Error extracting filename from URL: "+err.Error(), http.StatusInternalServerError)
		return
	}

	responseJSON, err := json.Marshal(map[string]interface{}{
		"can_download": canDownload,
		"download_url": URL,
		"filename":     fileName,
	})
	if err != nil {
		http.Error(w, "Error converting to JSON: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(responseJSON)
}

// FetchGravatarImageHandler is a handler for the /profiles/fetch_gravatar_image endpoint.
// It is used to fetch the Gravatar image for the user.
func DownloadGravatarImageHandler(w http.ResponseWriter, r *http.Request) {
	var data FetchGravatarData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		http.Error(w, "Error parsing JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	go DownloadGravatarImage(data)
	w.WriteHeader(http.StatusOK)
}

// DownloadGravatarImage is a function for downloading the Gravatar image of the creator.
// It first checks if Gravatar image is available locally. If not, it downloads it from the server.
// It preferes to download the image from the server using the Avatar128 parameter,
// but if it is not available, it tries to download it from Gravatar using gravatarHash.
func DownloadGravatarImage(data FetchGravatarData) {
	var url string
	if data.Avatar128 != "" {
		url = *Server + data.Avatar128
	} else {
		url = fmt.Sprintf("https://www.gravatar.com/avatar/%v?d=404", data.GravatarHash)
	}

	taskID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskID, "profiles/fetch_gravatar_image")

	filename := fmt.Sprintf("%d.jpg", data.ID)
	tempDir, err := GetSafeTempPath()
	if err != nil {
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err}
		return
	}

	gravatarPath := filepath.Join(tempDir, gravatar_dirname, filename)
	exists, _, _ := FileExists(gravatarPath)
	if exists {
		TaskFinishCh <- &TaskFinish{
			AppID:   data.AppID,
			TaskID:  taskID,
			Message: "Found on disk",
			Result:  map[string]string{"gravatar_path": gravatarPath},
		}
		return
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err}
		return
	}

	headers := getHeaders("", *SystemID, data.AddonVersion, data.PlatformVersion)
	req.Header = headers
	resp, err := ClientSmallThumbs.Do(req)
	if err != nil {
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("gravatar image download: %s, status (%s), query: %v", respString, resp.Status, url)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err}
		return
	}

	// Open the file for writing
	err = os.MkdirAll(filepath.Dir(gravatarPath), os.ModePerm)
	if err != nil {
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err}
		return
	}
	file, err := os.Create(gravatarPath)
	if err != nil {
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err}
		return
	}

	// Copy the response body to the file
	if _, err := io.Copy(file, resp.Body); err != nil {
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskID,
		Message: "Downloaded",
		Result:  map[string]string{"gravatar_path": gravatarPath},
	}
}

func GetUserProfileHandler(w http.ResponseWriter, r *http.Request) {
	var data MinimalTaskData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		http.Error(w, "Error parsing JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	go GetUserProfile(data)
	w.WriteHeader(http.StatusOK)
}

func GetUserProfile(data MinimalTaskData) {
	url := *Server + "/api/v1/me/"
	taskUUID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "profiles/get_user_profile")

	headers := getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		err = fmt.Errorf("get profile - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	req.Header = headers
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("get profile - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("get profile: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("get profile: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("get profile - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskUUID,
		Message: "data suceessfully fetched",
		Result:  respData,
	}
}

func GetRatingHandler(w http.ResponseWriter, r *http.Request) {
	var data GetRatingData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		http.Error(w, "Error parsing JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	go GetRating(data)
	w.WriteHeader(http.StatusOK)
}

// GetRating is a function for fetching the rating of the asset.
// Re-implements: file://daemon/daemon_ratings.py : get_rating()
func GetRating(data GetRatingData) {
	url := fmt.Sprintf("%s/api/v1/assets/%s/rating/", *Server, data.AssetID)
	taskUUID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "ratings/get_rating")

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		err = fmt.Errorf("get rating - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	req.Header = getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)

	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("get rating - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("get rating: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("get rating: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("get rating - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskUUID,
		Message: "Rating data obtained",
		Result:  respData,
	}
}

func SendRatingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var data SendRatingData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		es := fmt.Sprintf("error parsing JSON: %v", err)
		fmt.Println(es)
		http.Error(w, es, http.StatusBadRequest)
		return
	}
	go SendRating(data)
	w.WriteHeader(http.StatusOK)
}

// SendRating is a function for sending the user's rating of the asset.
// API documentation: https://www.blenderkit.com/api/v1/docs/#operation/assets_rating_update
func SendRating(data SendRatingData) {
	url := fmt.Sprintf("%s/api/v1/assets/%s/rating/%s/", *Server, data.AssetID, data.RatingType)
	taskUUID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "ratings/send_rating")

	reqData := map[string]interface{}{"score": data.RatingValue}
	reqBody, err := json.Marshal(reqData)
	if err != nil {
		err = fmt.Errorf("send rating - encoding: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var method string
	if data.RatingValue == 0 {
		method = http.MethodDelete
	} else {
		method = http.MethodPut
	}

	req, err := http.NewRequest(method, url, bytes.NewBuffer(reqBody))
	if err != nil {
		err = fmt.Errorf("send rating - making %v request: %w", method, err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	req.Header = getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("send rating - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if method == http.MethodDelete {
		if resp.StatusCode != http.StatusNoContent {
			_, respString, _ := ParseFailedHTTPResponse(resp)
			err := fmt.Errorf("remove rating - response (%v): %s at URL: %v", resp.Status, respString, url)
			TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
			return
		}

		TaskFinishCh <- &TaskFinish{
			AppID:   data.AppID,
			TaskID:  taskUUID,
			Message: fmt.Sprintf("Removed %s rating successfully", data.RatingType),
			Result:  map[string]string{},
		}
		return
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("send rating: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("send rating: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("send rating - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskUUID,
		Message: fmt.Sprintf("Rated %s=%.1f successfully", data.RatingType, data.RatingValue),
		Result:  respData,
	}
}

func GetBookmarksHandler(w http.ResponseWriter, r *http.Request) {
	var data MinimalTaskData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		es := fmt.Sprintf("error parsing JSON: %v", err)
		fmt.Println(es)
		http.Error(w, es, http.StatusBadRequest)
		return
	}

	go GetBookmarks(data)
	w.WriteHeader(http.StatusOK)
}

// GetBookmarks is a function for fetching the user's bookmarks.
func GetBookmarks(data MinimalTaskData) {
	url := fmt.Sprintf("%s/api/v1/search/?query=bookmarks_rating:1", *Server)
	taskUUID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "ratings/get_bookmarks")

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		err = fmt.Errorf("get boomarks - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	req.Header = getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("get bookmarks - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("get bookmarks: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("get bookmarks: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("get bookmarks - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskUUID,
		Message: "Bookmarks data obtained",
		Result:  respData,
	}
}

func GetCommentsHandler(w http.ResponseWriter, r *http.Request) {
	var data GetCommentsData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		es := fmt.Sprintf("error parsing JSON: %v", err)
		fmt.Println(es)
		http.Error(w, es, http.StatusBadRequest)
		return
	}

	go GetComments(data)
	w.WriteHeader(http.StatusOK)
}

// GetComments fetches all comments on the given asset.
//
// API documentation: https://www.blenderkit.com/api/v1/docs/#operation/comments_read
func GetComments(data GetCommentsData) {
	url := fmt.Sprintf("%s/api/v1/comments/assets-uuidasset/%s/", *Server, data.AssetID)
	taskUUID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "comments/get_comments")

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		err = fmt.Errorf("get comments - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	req.Header = getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("get comments - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("get comments: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("get comments: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("get comments - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskUUID,
		Message: "comments downloaded",
		Result:  respData,
	}
}

func CreateCommentHandler(w http.ResponseWriter, r *http.Request) {
	var data CreateCommentData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		es := fmt.Sprintf("error parsing JSON: %v", err)
		fmt.Println(es)
		http.Error(w, es, http.StatusBadRequest)
		return
	}
	go CreateComment(data)
	w.WriteHeader(http.StatusOK)
}

// CreateComment creates a comment on the given asset.
// It first GETs freshest comments data on the asset (from this we need Timestamp and SecurityHash for the POST request).
// It then creates a new comment through POST request.
//
// API docs GET: https://www.blenderkit.com/api/v1/docs/#operation/comments_get
//
// API docs POST: https://www.blenderkit.com/api/v1/docs/#operation/comments_comment_create
func CreateComment(data CreateCommentData) {
	get_url := fmt.Sprintf("%s/api/v1/comments/asset-comment/%s/", *Server, data.AssetID)
	post_url := fmt.Sprintf("%s/api/v1/comments/comment/", *Server)
	taskUUID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "comments/create_comment")

	req, err := http.NewRequest("GET", get_url, nil)
	if err != nil {
		err = fmt.Errorf("create comment - making GET request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	headers := getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	req.Header = headers
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("create comment - performing GET request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("create comment - GET: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("create comment - GET: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var commentsData GetCommentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&commentsData); err != nil {
		err = fmt.Errorf("create comment - decoding GET response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	uploadData := CommentPostData{
		Name:         "",
		Email:        "",
		URL:          "",
		Followup:     data.ReplyToID > 0,
		ReplyTo:      data.ReplyToID,
		Honeypot:     "",
		ContentType:  "assets.uuidasset",
		ObjectPK:     data.AssetID,
		Timestamp:    commentsData.Form.Timestamp,
		SecurityHash: commentsData.Form.SecurityHash,
		Comment:      data.CommentText,
	}
	uploadDataJSON, err := json.Marshal(uploadData)
	if err != nil {
		err = fmt.Errorf("create comment - encoding POST data: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	post_req, err := http.NewRequest("POST", post_url, bytes.NewBuffer(uploadDataJSON))
	if err != nil {
		err = fmt.Errorf("create comment - making POST request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	post_req.Header = headers
	post_resp, err := ClientAPI.Do(post_req)
	if err != nil {
		err = fmt.Errorf("create comment - performing POST request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("create comment - POST: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("create comment - POST: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(post_resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("create comment - decoding POST response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskUUID,
		Message: "Comment created",
		Result:  respData,
	}

	go GetComments(GetCommentsData{
		AppID:   data.AppID,
		APIKey:  data.APIKey,
		AssetID: data.AssetID,
	})
}

func FeedbackCommentHandler(w http.ResponseWriter, r *http.Request) {
	var data FeedbackCommentTaskData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		es := fmt.Sprintf("error parsing JSON: %v", err)
		fmt.Println(es)
		http.Error(w, es, http.StatusBadRequest)
		return
	}
	go FeedbackComment(data)
	w.WriteHeader(http.StatusOK)
}

// FeedbackComment uploads flag on the comment to the server.
// Flag is basically like/dislike but can be also a different flag.
//
// API docs: https://www.blenderkit.com/api/v1/docs/#operation/comments_feedback_create
func FeedbackComment(data FeedbackCommentTaskData) {
	url := fmt.Sprintf("%s/api/v1/comments/feedback/", *Server)
	taskUUID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "comments/feedback_comment")

	upload_data := FeedbackCommentData{
		CommentID: data.CommentID,
		Flag:      data.Flag,
	}

	JSON, err := json.Marshal(upload_data)
	if err != nil {
		err = fmt.Errorf("comment feedback - encoding data: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(JSON))
	if err != nil {
		err = fmt.Errorf("comment feedback - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	req.Header = getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("comment feedback - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("comment feedback: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("comment feedback: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("comment feedback - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskUUID,
		Message: "flag uploaded",
		Result:  respData,
	}
	go GetComments(GetCommentsData{
		AppID:   data.AppID,
		APIKey:  data.APIKey,
		AssetID: data.AssetID,
	})
}

func MarkCommentPrivateHandler(w http.ResponseWriter, r *http.Request) {
	var data MarkCommentPrivateTaskData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		es := fmt.Sprintf("error parsing JSON: %v", err)
		fmt.Println(es)
		http.Error(w, es, http.StatusBadRequest)
		return
	}
	go MarkCommentPrivate(data)
	w.WriteHeader(http.StatusOK)
}

// MarkCommentPrivate marks comment as private or public. Reported to user as "comment privacy".
//
// API docs: # https://www.blenderkit.com/api/v1/docs/#operation/comments_is_private_create
func MarkCommentPrivate(data MarkCommentPrivateTaskData) {
	url := fmt.Sprintf("%s/api/v1/comments/is_private/%d/", *Server, data.CommentID)
	taskUUID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "comments/mark_comment_private")

	uploadData := MarkCommentPrivateData{IsPrivate: data.IsPrivate}
	JSON, err := json.Marshal(uploadData)
	if err != nil {
		err = fmt.Errorf("comment privacy - encoding data: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(JSON))
	if err != nil {
		err = fmt.Errorf("comment privacy - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	req.Header = getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("comment privacy - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("comment privacy: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("comment privacy: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("comment privacy - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskUUID,
		Message: "comment visibility updated",
		Result:  respData,
	}
	go GetComments(GetCommentsData{
		AppID:   data.AppID,
		APIKey:  data.APIKey,
		AssetID: data.AssetID,
	})
}

func MarkNotificationReadHandler(w http.ResponseWriter, r *http.Request) {
	var data MarkNotificationReadTaskData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		es := fmt.Sprintf("error parsing JSON: %v", err)
		fmt.Println(es)
		http.Error(w, es, http.StatusBadRequest)
		return
	}
	go MarkNotificationRead(data)
	w.WriteHeader(http.StatusOK)
}

// MarkNotificationRead marks notification as read.
//
// API docs: https://www.blenderkit.com/api/v1/docs/#operation/notifications_mark-as-read_read
func MarkNotificationRead(data MarkNotificationReadTaskData) {
	url := fmt.Sprintf("%s/api/v1/notifications/mark-as-read/%d/", *Server, data.Notification)
	taskUUID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, taskUUID, "notifications/mark_notification_read")

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		err = fmt.Errorf("mark notification read - making request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	req.Header = getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	resp, err := ClientAPI.Do(req)
	if err != nil {
		err = fmt.Errorf("mark notification read - performing request: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("mark notification read: %s (%s)", respString, resp.Status)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	err = RespIsJSON(resp)
	if err != nil {
		err = fmt.Errorf("mark notification read: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		err = fmt.Errorf("mark notification read - decoding response: %w", err)
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskUUID, Error: err}
		return
	}
	TaskFinishCh <- &TaskFinish{
		AppID:   data.AppID,
		TaskID:  taskUUID,
		Message: "notification marked as read",
		Result:  respData,
	}
}

func assetUploadHandler(w http.ResponseWriter, r *http.Request) {
	var data AssetUploadRequestData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		es := fmt.Sprintf("error parsing JSON: %v", err)
		BKLog.Printf("%s AssetUploadHandler - %v", EmoError, es)
		http.Error(w, es, http.StatusBadRequest)
		return
	}
	go doAssetUpload(data)
	w.WriteHeader(http.StatusOK)
}

func doAssetUpload(data AssetUploadRequestData) {
	taskID := uuid.New().String()
	AddTaskCh <- &Task{
		AppID:    data.AppID,
		TaskID:   taskID,
		Data:     data,
		Result:   make(map[string]interface{}),
		TaskType: "asset_upload",
		Message:  "Upload initiated",
	}

	isMainFileUpload, isMetadataUpload, isThumbnailUpload := false, false, false
	for _, file := range data.UploadSet {
		if file == "MAINFILE" {
			isMainFileUpload = true
		}
		if file == "METADATA" {
			isMetadataUpload = true
		}
		if file == "THUMBNAIL" {
			isThumbnailUpload = true
		}
	}
	BKLog.Printf("%s Asset Upload Started - isMainFileUpload=%t isMetadataUpload=%t isThumbnailUpload=%t", EmoUpload, isMainFileUpload, isMetadataUpload, isThumbnailUpload)

	// 1. METADATA UPLOAD
	var metadataResp *AssetsCreateResponse
	var err error
	metadataID := uuid.New().String()
	AddTaskCh <- NewTask(data, data.AppID, metadataID, "asset_metadata_upload")

	if data.ExportData.AssetBaseID == "" { // 1.A NEW ASSET
		var respErrorJSON json.RawMessage
		metadataResp, respErrorJSON, err = CreateMetadata(data)
		if err != nil {
			TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err, Result: respErrorJSON}
			TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: metadataID, Error: err, Result: respErrorJSON}
			return
		}
	} else { // 1.B UPDATE OF ASSET
		if isMainFileUpload { // UPDATE OF MAINFILE -> DEVALIDATE ASSET
			data.UploadData.VerificationStatus = "uploading"
		}
		var respErrorJSON json.RawMessage
		metadataResp, respErrorJSON, err = UpdateMetadata(data)
		if err != nil {
			TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err, Result: respErrorJSON}
			TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: metadataID, Error: err, Result: respErrorJSON}
			return
		}
		metadataResp = FixAssetsUpdateResponse(metadataResp, data.ExportData.ID, data.UploadData.AssetType)
	}
	TaskFinishCh <- &TaskFinish{AppID: data.AppID, TaskID: metadataID, Result: metadataResp} // Assigns AssetID and AssetBaseID on the asset in Blender

	// 2. PACKING
	filesToUpload, err := PackBlendFile(data, *metadataResp, isMainFileUpload)
	if err != nil {
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err}
		return
	}

	// 3. UPLOAD
	errJSON, err := UploadAssetData(filesToUpload, data, *metadataResp, isMainFileUpload, taskID)
	if err != nil {
		TaskErrorCh <- &TaskError{AppID: data.AppID, TaskID: taskID, Error: err, Result: errJSON}
		return
	}

	// 4. COMPLETE
	TaskFinishCh <- &TaskFinish{AppID: data.AppID, TaskID: taskID, Result: *metadataResp, Message: "Upload successful!"}
}

type CompleteUploadFileBlockingData struct {
	AppID           int    `json:"app_id"`
	APIKey          string `json:"api_key"`
	AddonVersion    string `json:"addon_version"`
	PlatformVersion string `json:"platform_version"`

	AssetID          string `json:"assetId"`
	FileType         string `json:"fileType"`
	FileIndex        int    `json:"fileIndex"`
	FilePath         string `json:"filePath"`
	OriginalFilename string `json:"originalFilename"`
}

// Complete upload file in one blocking request. Used by background scripts.
func CompleteUploadFileBlocking(w http.ResponseWriter, r *http.Request) {
	var data CompleteUploadFileBlockingData
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var minimalData = MinimalTaskData{
		APIKey:          data.APIKey,
		AddonVersion:    data.AddonVersion,
		PlatformVersion: data.PlatformVersion,
	}

	fileData := UploadFile{
		Type:     data.FileType,
		Index:    data.FileIndex,
		FilePath: data.FilePath,
	}

	uploadInfo, err := get_S3_upload_JSON(fileData, minimalData, data.AssetID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Println("CompleteUploadFileBlocking uploading file to S3")
	err = uploadFileToS3(fileData, uploadInfo, 0, "", data.APIKey, data.AddonVersion, data.PlatformVersion)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	fmt.Println("CompleteUploadFileBlocking S3 upload complete")
	w.WriteHeader(http.StatusOK)
}

// AssetUploadData uploads asset data to S3. If response is not OK, it will return the JSON of the error response and error.
func UploadAssetData(files []UploadFile, data AssetUploadRequestData, metadataResp AssetsCreateResponse, isMainFileUpload bool, taskID string) (json.RawMessage, error) {
	for _, file := range files { // will be empty if only metadata is uploaded
		var minimalTaskData = MinimalTaskData{
			AppID:           data.AppID,
			APIKey:          data.Preferences.APIKey,
			AddonVersion:    data.UploadData.AddonVersion,
			PlatformVersion: data.UploadData.PlatformVersion,
		}
		upload_info_json, err := get_S3_upload_JSON(file, minimalTaskData, metadataResp.ID)
		if err != nil {
			return nil, err
		}

		err = uploadFileToS3(file, upload_info_json, data.AppID, taskID, data.Preferences.APIKey, data.UploadData.AddonVersion, data.UploadData.PlatformVersion)
		if err != nil {
			return nil, err
		}
	}

	// Check the status if only thumbnail or metadata gets reuploaded.
	// the logic is that on hold assets might be switched to uploaded state for validators,
	// if the asset was put on hold because of thumbnail only.
	set_uploaded_status := false
	if !isMainFileUpload {
		if metadataResp.VerificationStatus == "on_hold" {
			set_uploaded_status = true
		}
		if metadataResp.VerificationStatus == "deleted" {
			set_uploaded_status = true
		}
		if metadataResp.VerificationStatus == "rejected" {
			set_uploaded_status = true
		}
	}

	if isMainFileUpload {
		set_uploaded_status = true
	}

	if !set_uploaded_status {
		return nil, nil
	}

	// mark on server as uploaded
	confirm_data := map[string]string{"verificationStatus": "uploaded"}
	confirm_data_json, err := json.Marshal(confirm_data)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/api/v1/assets/%s/", *Server, metadataResp.ID)
	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(confirm_data_json))
	if err != nil {
		return nil, err
	}
	req.Header = getHeaders(data.Preferences.APIKey, *SystemID, data.UploadData.AddonVersion, data.UploadData.PlatformVersion)

	resp, err := ClientAPI.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg := "asset status update failed"
		respJSON, respString, respErr := ParseFailedHTTPResponse(resp)
		if respErr != nil || respJSON == nil {
			return nil, fmt.Errorf("%s (%s): failed parsing error response (%v), [URL: %v]", msg, resp.Status, respString, url)
		}
		return respJSON, fmt.Errorf("%s (%s)", msg, resp.Status)
	}

	return nil, nil
}

func get_S3_upload_JSON(file UploadFile, data MinimalTaskData, assetID string) (S3UploadInfoResponse, error) {
	var resp_JSON S3UploadInfoResponse
	upload_info := map[string]interface{}{
		"assetId":          assetID,
		"fileType":         file.Type,
		"fileIndex":        file.Index,
		"originalFilename": filepath.Base(file.FilePath),
	}
	upload_info_json, err := json.Marshal(upload_info)
	if err != nil {
		return resp_JSON, err
	}

	url := fmt.Sprintf("%s/api/v1/uploads/", *Server)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(upload_info_json))
	if err != nil {
		return resp_JSON, err
	}
	req.Header = getHeaders(data.APIKey, *SystemID, data.AddonVersion, data.PlatformVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ClientAPI.Do(req)
	if err != nil {
		return resp_JSON, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		err := fmt.Errorf("status code error (%d) %s: %v", resp.StatusCode, resp.Status, respString)
		return resp_JSON, err
	}

	resp_json, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp_JSON, err
	}

	err = json.Unmarshal(resp_json, &resp_JSON)
	if err != nil {
		return resp_JSON, err
	}

	return resp_JSON, nil
}

// Struct to track progress of a file upload/download
type ProgressReader struct {
	r          io.Reader // The underlying reader
	n          int64     // Number of bytes already read
	total      int64     // Total byte size of the file
	appID      int       // which app is this for - used for sending progress updates via TaskMessageCh
	taskID     string    // which task is this for - used for sending progress updates via TaskMessageCh
	preMessage string    // message to prepend to the progress message
}

// Read reads data into p, tracking bytes read to report progress.
func (pr *ProgressReader) Read(p []byte) (int, error) {
	read, err := pr.r.Read(p)
	pr.n += int64(read)

	// Calculate and send the progress percentage
	percentage := float64(pr.n) / float64(pr.total) * 100
	msg := fmt.Sprintf("%s: %d%%", pr.preMessage, int(percentage))

	if pr.appID != 0 || pr.taskID != "" { // bg_scripts don't have access to TaskMessageCh now, TODO: implement Task to be used with BG_scripts
		TaskMessageCh <- &TaskMessageUpdate{AppID: pr.appID, TaskID: pr.taskID, Message: msg}
	}

	return read, err
}

type UploadValidationResponse struct {
	Detail string `json:"detail"`
}

func uploadFileToS3(file UploadFile, uploadInfo S3UploadInfoResponse, appID int, taskID, apiKey, addonVersion, platformVersion string) error {
	fileInfo, err := os.Stat(file.FilePath)
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}
	fileSize := fileInfo.Size()

	fileContent, err := os.Open(file.FilePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer fileContent.Close()

	// Wrap the fileContent with our ProgressReader
	progressReader := &ProgressReader{
		r:          fileContent,
		total:      fileSize,
		appID:      appID,
		taskID:     taskID,
		preMessage: fmt.Sprintf("Uploading %s", file.Type),
	}

	req, err := http.NewRequest("PUT", uploadInfo.S3UploadURL, progressReader)
	if err != nil {
		return fmt.Errorf("failed to create S3 upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = fileSize

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		_, respString, _ := ParseFailedHTTPResponse(resp)
		return fmt.Errorf("S3 upload failed (%d): %s", resp.StatusCode, respString)
	}

	// UPLOAD VALIDATION
	valReq, err := http.NewRequest("POST", uploadInfo.UploadDoneURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create upload validation request: %w", err)
	}
	valReq.Header = getHeaders(apiKey, *SystemID, addonVersion, platformVersion)

	valResp, err := ClientAPI.Do(valReq)
	if err != nil {
		return fmt.Errorf("failed to validate upload with server: %w", err)
	}
	defer valResp.Body.Close()

	if valResp.StatusCode >= 400 {
		valRespData, err := io.ReadAll(valResp.Body)
		if err != nil {
			return fmt.Errorf("upload validation faild (%d) AND failed to read upload validation response: %w", valResp.StatusCode, err)
		}
		var valRespJSON UploadValidationResponse
		err = json.Unmarshal(valRespData, &valRespJSON)
		if err != nil {
			return fmt.Errorf("upload validation failed (%d) AND failed to unmarshal upload validation response to JSON: %w", valResp.StatusCode, err)
		}
		return fmt.Errorf("upload validation failed (%d): %v", valResp.StatusCode, valRespJSON.Detail)
	}

	return nil
}

func PackBlendFile(data AssetUploadRequestData, metadata AssetsCreateResponse, isMainFileUpload bool) ([]UploadFile, error) {
	files := []UploadFile{}
	addon_path := data.Preferences.AddonDir
	blenderUserScripts := filepath.Dir(filepath.Dir(addon_path)) // e.g.: /Users/username/Library/Application Support/Blender/4.1/scripts"
	script_path := filepath.Join(addon_path, "upload_bg.py")
	cleanfile_path := filepath.Join(addon_path, cleanfile_path)

	upload_data := metadata
	export_data := data.ExportData
	upload_set := data.UploadSet

	if export_data.AssetBaseID == "" {
		export_data.AssetBaseID = metadata.AssetBaseID
		export_data.ID = metadata.ID
	}
	upload_data.AssetBaseID = export_data.AssetBaseID
	upload_data.ID = export_data.ID

	var fpath string
	if isMainFileUpload { // This should be a separate function!
		if upload_data.AssetType == "hdr" {
			fpath = export_data.HDRFilepath
		} else {
			fpath = filepath.Join(export_data.TempDir, export_data.AssetBaseID+".blend")
			data := PackingData{
				ExportData: export_data,
				UploadData: upload_data,
				UploadSet:  upload_set,
			}
			datafile := filepath.Join(export_data.TempDir, "data.json")
			log.Println("opening file @ PackBlendFile()")

			JSON, err := json.Marshal(data)
			if err != nil {
				log.Fatal(err)
			}

			err = os.WriteFile(datafile, JSON, 0644)
			if err != nil {
				log.Fatal(err)
			}
			log.Println("Running asset packing")
			cmd := exec.Command(
				export_data.BinaryPath,
				"--background",
				"--factory-startup", // disables user preferences, addons, etc.
				"--addons",
				"blenderkit",
				"-noaudio",
				cleanfile_path,
				"--python",
				script_path,
				"--",
				datafile,
			)

			cmd.Env = append(os.Environ(), fmt.Sprintf("BLENDER_USER_SCRIPTS=%v", blenderUserScripts))
			out, err := cmd.CombinedOutput()
			color.FgGray.Println("(Background) Packing logs:\n", string(out))
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode := exitErr.ExitCode()
					return files, fmt.Errorf("command exited with code %d\nOutput: %s", exitCode, out)
				} else {
					return files, fmt.Errorf("command execution failed: %w\nOutput: %s", err, out)
				}
			}
		}

		exists, _, _ := FileExists(fpath)
		if !exists {
			return files, fmt.Errorf("packed file (%s) does not exist, please try manual packing first", fpath)
		}
	}

	for _, filetype := range upload_set {
		if filetype == "THUMBNAIL" {
			file := UploadFile{
				Type:     "thumbnail",
				Index:    0,
				FilePath: export_data.ThumbnailPath,
			}
			files = append(files, file)
			continue
		}

		if filetype == "MAINFILE" {
			file := UploadFile{
				Type:     "blend",
				Index:    0,
				FilePath: fpath,
			}
			files = append(files, file)
			continue
		}

	}

	return files, nil
}

// CreateMetadata creates metadata on the server, so it can be saved inside the current file.
// API docs: https://www.blenderkit.com/api/v1/docs/#tag/assets/operation/assets_create
func CreateMetadata(data AssetUploadRequestData) (*AssetsCreateResponse, json.RawMessage, error) {
	url := fmt.Sprintf("%s/api/v1/assets/", *Server)
	headers := getHeaders(data.Preferences.APIKey, *SystemID, data.UploadData.AddonVersion, data.UploadData.PlatformVersion)

	parameters, ok := data.UploadData.Parameters.(map[string]interface{})
	if !ok {
		return nil, nil, fmt.Errorf("parameters is not a map[string]interface{}")
	}
	data.UploadData.Parameters = DictToParams(parameters)

	JSON, err := json.Marshal(data.UploadData)
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(JSON))
	if err != nil {
		return nil, nil, err
	}

	req.Header = headers
	resp, err := ClientAPI.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		msg := "error creating asset"
		respJSON, respString, err := ParseFailedHTTPResponse(resp)
		if err != nil || respJSON == nil {
			return nil, nil, fmt.Errorf("%s (%s): failed parsing error response (%v), [URL: %v]", msg, resp.Status, respString, url)
		}
		return nil, respJSON, fmt.Errorf("%s (%s)", msg, resp.Status)
	}

	respData := new(AssetsCreateResponse)
	if err := json.NewDecoder(resp.Body).Decode(respData); err != nil {
		return nil, nil, err
	}

	return respData, nil, nil
}

// UploadMetadata uploads metadata to the server, so it can be saved inside the current file.
// API docs: https://www.blenderkit.com/api/v1/docs/#tag/assets/operation/assets_update
func UpdateMetadata(data AssetUploadRequestData) (*AssetsCreateResponse, json.RawMessage, error) {
	url := fmt.Sprintf("%s/api/v1/assets/%s/", *Server, data.ExportData.ID)
	headers := getHeaders(data.Preferences.APIKey, *SystemID, data.UploadData.AddonVersion, data.UploadData.PlatformVersion)

	parameters, ok := data.UploadData.Parameters.(map[string]interface{})
	if !ok {
		return nil, nil, fmt.Errorf("parameters is not a map[string]interface{}")
	}
	data.UploadData.Parameters = DictToParams(parameters)

	JSON, err := json.Marshal(data.UploadData)
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(JSON))
	if err != nil {
		return nil, nil, err
	}

	req.Header = headers
	resp, err := ClientAPI.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg := "metadata update failed"
		respJSON, respString, respErr := ParseFailedHTTPResponse(resp)
		if respErr != nil || respJSON == nil {
			return nil, nil, fmt.Errorf("%s (%s): failed parsing error response (%v), [URL: %v]", msg, resp.Status, respString, url)
		}
		return nil, respJSON, fmt.Errorf("%s (%s)", msg, resp.Status)
	}

	respData := new(AssetsCreateResponse)
	if err := json.NewDecoder(resp.Body).Decode(respData); err != nil {
		return nil, nil, err
	}

	return respData, nil, nil
}

// Try to parse the error response from the server.
// Returns:
//
// json.RawMessage: the json response from the server, if it was valid JSON.
//
// string: the response from the server as string, for json and non-json responses.
//
// error: the error which occurred while parsing the response (resp could not be read)
func ParseFailedHTTPResponse(resp *http.Response) (json.RawMessage, string, error) {
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		BKLog.Printf("%v Failed request on %v, error response: %v", EmoWarning, resp.Request.URL, err)
		return nil, "", err
	}
	bodyString := string(bodyBytes)
	if !json.Valid(bodyBytes) {
		// API rate limit exceeded
		if strings.Contains(bodyString, "error code: 1015") {
			msg := fmt.Sprintf("API rate limit exceeded, wait for a while [%v]", bodyString)
			return nil, msg, fmt.Errorf("invalid json")
		}

		// General error
		BKLog.Printf("%v Failed request on %v, error response: %v", EmoWarning, resp.Request.URL, bodyString)
		return nil, bodyString, fmt.Errorf("invalid json")
	}

	var JSON json.RawMessage
	if err := json.Unmarshal(bodyBytes, &JSON); err != nil {
		BKLog.Printf("%v Failed request on %v, error response: %v", EmoWarning, resp.Request.URL, bodyString)
		return nil, bodyString, fmt.Errorf("error unmarshalling error response: %w", err)
	}

	BKLog.Printf("%v Failed request on %v, error response: %v", EmoWarning, resp.Request.URL, string(JSON))
	return JSON, bodyString, nil
}

// DictToParams (in Python terminology) converts a map of inputs into a slice of parameter objects.
// This is used to convert the parameters from the add-on to the format expected by the API.
// e.g. {"a": "1", "b": "2"} -> [{"parameterType": "a", "value": "1"}, {"parameterType": "b", "value": "2"}]
func DictToParams(inputs map[string]interface{}) []map[string]string {
	parameters := make([]map[string]string, 0)
	for k, v := range inputs {
		var value string
		switch v := v.(type) {
		case []string:
			for idx, s := range v {
				value += s
				if idx < len(v)-1 {
					value += ","
				}
			}
		case bool:
			value = fmt.Sprintf("%t", v)
		case int, int32, int64: // Integers has to be converted via %d to avoid scientific notation
			value = fmt.Sprintf("%d", v)
		case float32: // Floats has to be converted via FormatFloat to avoid scientific notation and trailing zeros
			value = strconv.FormatFloat(float64(v), 'f', -1, 32)
		case float64:
			value = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			value = fmt.Sprintf("%v", v)
		}
		param := map[string]string{
			"parameterType": k,
			"value":         value,
		}
		parameters = append(parameters, param)
	}
	return parameters
}
