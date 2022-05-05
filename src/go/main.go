package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gookit/color"
	"github.com/oov/audio/wave"
	"github.com/yuin/gluare"
	lua "github.com/yuin/gopher-lua"
)

const maxRetry = 10
const maxStay = 20
const resendProtectDuration = 5 * time.Second
const writeNotificationDeadline = 5 * time.Second

var verbose bool
var preventClear bool
var version string

type file struct {
	Filepath string
	Hash     string
	ModDate  time.Time
	TryCount int
}

type fileState struct {
	Retry int
	Stay  int
}

type sentFileState struct {
	At   time.Time
	Hash string
}

type colorizer interface {
	Renderln(a ...interface{}) string
	Sprintf(format string, args ...interface{}) string
}

type dummyColorizer struct{}

func (dummyColorizer) Renderln(a ...interface{}) string {
	if len(a) == 0 {
		return ""
	}
	if len(a) == 1 {
		return fmt.Sprint(a...)
	}
	r := fmt.Sprintln(a...)
	return r[:len(r)-1]
}

func (dummyColorizer) Sprintf(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}

var (
	warn     colorizer = color.Yellow
	caption  colorizer = color.Bold
	suppress colorizer = color.Gray
)

func clearScreen() error {
	cmd := exec.Command("cmd", "/c", "cls")
	cmd.Stdout = os.Stdout
	return cmd.Run()
}

func verifyAndCalcHash(wavPath string, txtPath string) (string, error) {
	txt, err := os.OpenFile(txtPath, os.O_RDWR, 0666)
	if err != nil {
		return "", fmt.Errorf("无法打开文本文件: %w", err)
	}
	defer txt.Close()
	wav, err := os.OpenFile(wavPath, os.O_RDWR, 0666)
	if err != nil {
		return "", fmt.Errorf("无法打开wav文件: %w", err)
	}
	defer wav.Close()
	r, wfe, err := wave.NewLimitedReader(wav)
	if err != nil {
		return "", fmt.Errorf("无法读取wav文件: %w", err)
	}
	if r.N == 0 || wfe.Format.SamplesPerSec == 0 || wfe.Format.Channels == 0 || wfe.Format.BitsPerSample == 0 {
		return "", fmt.Errorf("wav文件保存值无效: %w", err)
	}
	if _, err := wav.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("无法移动wav文件读取光标: %w", err)
	}
	h := fnv.New32a()
	if _, err := io.Copy(h, wav); err != nil {
		return "", fmt.Errorf("无法读取wav文件: %w", err)
	}
	h2 := fnv.New32a()
	if _, err := io.Copy(h2, txt); err != nil {
		return "", fmt.Errorf("无法读取文本文件: %w", err)
	}
	return string(h2.Sum(h.Sum(nil))), nil
}

func processFiles(L *lua.LState, files []file, sort string, recentChanged map[string]fileState, recentSent map[string]sentFileState) (needRetry bool, err error) {
	var errStay error
	defer func() {
		for k, ct := range recentChanged {
			if ct.Retry == maxRetry-1 || ct.Stay == maxStay-1 {
				log.Println(warn.Renderln("  出错过多，暂时放弃此文件:", k))
				delete(recentChanged, k)
				continue
			}
			if errStay != nil {
				ct.Stay += 1
			} else {
				ct.Retry += 1
			}
			recentChanged[k] = ct
			needRetry = true
		}
	}()

	proj, err := readGCMZDropsData()
	if err != nil {
		if verbose {
			log.Println(suppress.Renderln("工程信息获取失败:", err))
		}
		err = fmt.Errorf("找不到装有‘随意拖放’ v0.3.13 或更高版本的 AviUtl")
		return
	}
	if proj.Width == 0 {
		err = fmt.Errorf("找不到AviUtl中正在编辑的工程文件")
		return
	}
	t := L.NewTable()
	for _, f := range files {
		file := L.NewTable()
		file.RawSetString("path", lua.LString(f.Filepath))
		file.RawSetString("hash", lua.LString(f.Hash))
		file.RawSetString("trycount", lua.LNumber(f.TryCount))
		file.RawSetString("maxretry", lua.LNumber(maxRetry))
		file.RawSetString("moddate", lua.LNumber(float64(f.ModDate.Unix())+(float64(f.ModDate.Nanosecond())/1e9)))
		t.Append(file)
	}
	pt := L.NewTable()
	pt.RawSetString("projectfile", lua.LString(proj.ProjectFile))
	pt.RawSetString("gcmzapiver", lua.LNumber(proj.GCMZAPIVer))
	pt.RawSetString("flags", lua.LNumber(proj.Flags))
	pt.RawSetString("flags_englishpatched", lua.LBool(proj.Flags&1 == 1))
	pt.RawSetString("window", lua.LNumber(proj.Window))
	pt.RawSetString("width", lua.LNumber(proj.Width))
	pt.RawSetString("height", lua.LNumber(proj.Height))
	pt.RawSetString("video_rate", lua.LNumber(proj.VideoRate))
	pt.RawSetString("video_scale", lua.LNumber(proj.VideoScale))
	pt.RawSetString("audio_rate", lua.LNumber(proj.AudioRate))
	pt.RawSetString("audio_ch", lua.LNumber(proj.AudioCh))
	if err = L.CallByParam(lua.P{
		Fn:      L.GetGlobal("changed"),
		NRet:    1,
		Protect: true,
	}, t, lua.LString(sort), pt); err != nil {
		return
	}
	rv := L.ToTable(-1)
	if rv == nil {
		err = fmt.Errorf("处理后返回值异常")
		return
	}
	// remove processed entries
	now := time.Now()
	n := rv.MaxN()
	for i := 1; i <= n; i++ {
		tbl, ok := rv.RawGetInt(i).(*lua.LTable)
		if !ok {
			continue
		}
		// remove it from the candidate list regardless of success or failure.
		src := tbl.RawGetString("src").String()
		hash := tbl.RawGetString("hash").String()
		delete(recentChanged, src)
		recentSent[src] = sentFileState{
			At:   now,
			Hash: hash,
		}

		destV := tbl.RawGetString("dest")
		if destV.Type() != lua.LTString {
			continue // rule not found
		}
		// if TTS software creates files in the same location as the project,
		// files that have already been processed may be subject to processing again.
		// put dest on recentSent to prevent it.
		dest := destV.String()
		recentSent[dest] = sentFileState{
			At:   now,
			Hash: hash,
		}
		delete(recentChanged, dest)
	}
	L.Pop(1)
	return
}

func getProjectPath() string {
	proj, err := readGCMZDropsData()
	if err != nil {
		return ""
	}
	if proj.Width == 0 {
		return ""
	}
	if proj.GCMZAPIVer < 1 {
		return ""
	}
	return proj.ProjectFile
}

func watchProjectPath(ctx context.Context, notify chan<- map[string]struct{}, projectPath string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			if projectPath != getProjectPath() {
				if verbose {
					log.Println(suppress.Renderln("  检测到 AviUtl 工程路径更改"))
				}
				notify <- nil
				return
			}
		}
	}
}

func watch(ctx context.Context, watcher *fsnotify.Watcher, settingWatcher *fsnotify.Watcher, notify chan<- map[string]struct{}, settingFile string, freshness float64, sortdelay float64) {
	defer close(notify)
	var finish bool
	changed := map[string]struct{}{}
	timer := time.NewTimer(time.Duration(sortdelay) * time.Second)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-watcher.Events:
			if verbose {
				log.Println(suppress.Renderln("事件校验:", event))
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				if verbose {
					log.Println(suppress.Renderln("  操作非Create / Write，因此均不执行。"))
				}
				continue
			}
			ext := strings.ToLower(filepath.Ext(event.Name))
			if ext != ".wav" && ext != ".txt" {
				if verbose {
					log.Println(suppress.Renderln("  非*.wav / *.txt文件，因此均不执行。"))
				}
				continue
			}
			st, err := os.Stat(event.Name)
			if err != nil {
				if verbose {
					log.Println(suppress.Renderln("  更新日期获取失败，因此均不执。。"))
				}
				continue
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				// Even if freshness == 0, verify when notified by Write.
				// Because Write notification is also sent in the case of a file attribute change or modify zone identifier.
				// See: https://github.com/oov/forcepser/issues/10
				if time.Since(st.ModTime()) > writeNotificationDeadline {
					if verbose {
						log.Println(suppress.Renderln("  有文件更改通知，但更新时间早于", writeNotificationDeadline, "因此均不执行"))
					}
					continue
				}
			} else {
				if freshness > 0 {
					if math.Abs(time.Since(st.ModTime()).Seconds()) > freshness {
						if verbose {
							log.Println(suppress.Renderln("  更新日期为一秒前", freshness, "因此均不执行"))
						}
						continue
					}
				}
			}
			if verbose {
				log.Println(suppress.Renderln("  选定为传输文件候选"))
			}
			changed[event.Name[:len(event.Name)-len(ext)]+".wav"] = struct{}{}
			timer.Reset(time.Duration(sortdelay) * time.Second)
		case event := <-settingWatcher.Events:
			if event.Name == settingFile {
				if verbose {
					log.Println(suppress.Renderln("  处理为重新读取配置文件。"))
				}
				finish = true
				timer.Reset(100 * time.Millisecond)
				continue
			}
		case err := <-watcher.Errors:
			log.Println(warn.Renderln("监视时发生错误:", err))
		case err := <-settingWatcher.Errors:
			log.Println(warn.Renderln("监视时发生错误:", err))
		case <-timer.C:
			if finish {
				notify <- nil
				continue
			}
			notify <- changed
			changed = map[string]struct{}{}
		}
	}
}

func bool2str(b bool, t string, f string) string {
	if b {
		return t
	}
	return f
}

func printDetails(setting *setting, tempDir string) {
	log.Println(caption.Renderln("AviUtl 工程信息:"))
	proj, err := readGCMZDropsData()
	if err != nil {
		log.Println(warn.Renderln("  找不到装有‘随意拖放’ v0.3.13 或更高版本的 AviUtl"))
	} else {
		if proj.Width == 0 {
			log.Println(warn.Renderln("  找不到AviUtl中正在编辑的工程文件"))
		} else {
			if proj.GCMZAPIVer < 1 {
				log.Println(warn.Renderln("  随意拖放版本过旧，部分功能无法使用。"))
			} else {
				log.Println(suppress.Renderln("  ProjectFile:"), proj.ProjectFile)
			}
			log.Println(suppress.Renderln("  Window:     "), int(proj.Window))
			log.Println(suppress.Renderln("  Width:      "), proj.Width)
			log.Println(suppress.Renderln("  Height:     "), proj.Height)
			log.Println(suppress.Renderln("  VideoRate:  "), proj.VideoRate)
			log.Println(suppress.Renderln("  VideoScale: "), proj.VideoScale)
			log.Println(suppress.Renderln("  AudioRate:  "), proj.AudioRate)
			log.Println(suppress.Renderln("  AudioCh:    "), proj.AudioCh)
			if proj.GCMZAPIVer >= 2 {
				log.Println(suppress.Renderln("  Flags:      "), proj.Flags)
			}
			log.Println()
		}
	}

	log.Println(caption.Renderln("环境变量:"))
	log.Println(suppress.Renderln("  %BASEDIR%:   "), setting.BaseDir)
	log.Println(suppress.Renderln("  %TEMPDIR%:   "), tempDir)
	log.Println(suppress.Renderln("  %PROJECTDIR%:"), setting.projectDir)
	log.Println(suppress.Renderln("  %PROFILE%:   "), getSpecialFolderPath(CSIDL_PROFILE))
	log.Println(suppress.Renderln("  %DESKTOP%:   "), getSpecialFolderPath(CSIDL_DESKTOP))
	log.Println(suppress.Renderln("  %MYDOC%:     "), getSpecialFolderPath(CSIDL_PERSONAL))
	log.Println()

	log.Println(suppress.Renderln("  delta:"), setting.Delta)
	log.Println(suppress.Renderln("  freshness:"), setting.Freshness)
	log.Println()

	for i, a := range setting.Asas {
		log.Println(caption.Sprintf("Asas %d:", i+1))
		log.Println(suppress.Renderln("  目标EXE:"), a.Exe)
		log.Println(suppress.Renderln("  滤镜:"), a.Filter)
		log.Println(suppress.Renderln("  保存目录:"), a.ExpandedFolder())
		log.Println(suppress.Renderln("  格式:"), a.Format)
		log.Println(suppress.Renderln("  标记:"), a.Flags)
		if !a.Exists() {
			log.Println(warn.Renderln("  [警告] 找不到目标EXE，因此忽略设定"))
		}
	}
	log.Println()
	for i, r := range setting.Rule {
		log.Println(caption.Sprintf("规则%d:", i+1))
		log.Println(suppress.Renderln("  目标文件夹:"), r.ExpandedDir())
		log.Println(suppress.Renderln("  目标文件名:"), r.File)
		log.Println(suppress.Renderln("  文本文件字符编码:"), r.Encoding)
		if r.textRE != nil {
			log.Println(suppress.Renderln("  文本判断正则表达式:"), r.Text)
		}
		log.Println(suppress.Renderln("  插入目标图层:"), r.Layer)
		log.Println(suppress.Renderln("  modifier:"), bool2str(r.Modifier != "", "是", "否"))
		log.Println(suppress.Renderln("  用户数据:"), r.UserData)
		log.Println(suppress.Renderln("  填充:"), r.Padding)
		log.Println(suppress.Renderln("  EXO文件:"), r.ExoFile)
		log.Println(suppress.Renderln("  Lua文件:"), r.LuaFile)
		log.Println(suppress.Renderln("  移动wav文件:"), r.FileMove.Readable())
		if r.FileMove != "off" {
			log.Println(suppress.Sprintf("    %s目标:", r.FileMove.Readable()), r.ExpandedDestDir())
		}
		log.Println(suppress.Renderln("  删除文本文件:"), bool2str(r.DeleteText, "是", "否"))
		if !r.ExistsDir() {
			log.Println(warn.Renderln("  [警告] 找不到目标文件夹，因此忽略设定"))
		}
	}
	log.Println()
}

func loadSetting(path string, tempDir string, projectDir string) (*setting, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return newSetting(f, tempDir, projectDir)
}

func tempSetting(tempDir string, projectDir string) (*setting, error) {
	return newSetting(strings.NewReader(``), tempDir, projectDir)
}

func process(watcher *fsnotify.Watcher, settingWatcher *fsnotify.Watcher, settingFile string, recentChanged map[string]fileState, recentSent map[string]sentFileState, loop int) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法获取exe文件路径: %w", err)
	}
	tempDir := filepath.Join(filepath.Dir(exePath), "tmp")
	if err = os.Mkdir(tempDir, 0777); err != nil && !os.IsExist(err) {
		return fmt.Errorf("tmp 文件夹创建失败: %w", err)
	}

	projectPath := getProjectPath()
	var projectDir string
	if projectPath != "" {
		projectDir = filepath.Dir(projectPath)
	}

	setting, err := loadSetting(settingFile, tempDir, projectDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("设定加载失败: %w", err)
		}
		log.Println(warn.Renderln("无法打开配置文件。"))
		log.Println(suppress.Renderln(filepath.Base(settingFile), "创建后会自动加载。"))
		log.Println()
		setting, _ = tempSetting(tempDir, projectDir)
	} else {
		printDetails(setting, tempDir)
	}

	L := lua.NewState()
	defer L.Close()

	L.PreloadModule("re", gluare.Loader)
	err = L.DoString(`re = require("re")`)
	if err != nil {
		return fmt.Errorf("脚本环境初始化出错: %w", err)
	}

	L.SetGlobal("debug_print", L.NewFunction(luaDebugPrint))
	L.SetGlobal("debug_error", L.NewFunction(luaDebugError))
	L.SetGlobal("debug_print_verbose", L.NewFunction(luaDebugPrintVerbose))
	L.SetGlobal("sendfile", L.NewFunction(luaSendFile))
	L.SetGlobal("findrule", L.NewFunction(luaFindRule(setting)))
	L.SetGlobal("getaudioinfo", L.NewFunction(luaGetAudioInfo))
	L.SetGlobal("tosjis", L.NewFunction(luaToSJIS))
	L.SetGlobal("fromsjis", L.NewFunction(luaFromSJIS))
	L.SetGlobal("toexostring", L.NewFunction(luaToEXOString))
	L.SetGlobal("fromexostring", L.NewFunction(luaFromEXOString))
	L.SetGlobal("tofilename", L.NewFunction(luaToFilename))
	L.SetGlobal("replaceenv", L.NewFunction(luaReplaceEnv(setting)))

	if err := L.DoFile("_entrypoint.lua"); err != nil {
		return fmt.Errorf("运行 _entrypoint.lua 时出错: %w", err)
	}

	updateOnly := loop > 0
	for _, a := range setting.Asas {
		if a.Exists() {
			if _, err := a.ConfirmAndRun(updateOnly); err != nil {
				return fmt.Errorf("程序启动失败: %w", err)
			}
		}
	}

	log.Println(caption.Sprintf("开始监视:"))
	watching := 0
	for _, dir := range setting.Dirs() {
		err = watcher.Add(dir)
		if err != nil {
			return fmt.Errorf("无法监视文件夹 %s : %w", dir, err)
		}
		log.Println("  " + dir)
		watching++
		defer watcher.Remove(dir)
	}
	if watching == 0 {
		log.Println(warn.Renderln("  [警告] 无可供监视的文件夹。"))
	}
	notify := make(chan map[string]struct{}, 10000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchProjectPath(ctx, notify, projectPath)
	go watch(ctx, watcher, settingWatcher, notify, settingFile, setting.Freshness, setting.SortDelay)
	timer := time.NewTimer(time.Duration(setting.SortDelay) * time.Second)
	timer.Stop()
	timerAt := time.Now()
	for {
		select {
		case updatedFiles, ok := <-notify:
			if !ok {
				return nil
			}
			if updatedFiles == nil {
				if preventClear {
					return nil
				}
				return clearScreen()
			}
			now := time.Now()
			for k, st := range recentSent {
				if now.Sub(st.At) > resendProtectDuration {
					delete(recentSent, k)
				}
			}
			for k := range updatedFiles {
				if _, ok := recentChanged[k]; !ok {
					recentChanged[k] = fileState{}
				}
			}
			d := time.Duration(setting.SortDelay) * time.Second
			at := time.Now().Add(d)
			if at.After(timerAt) {
				timerAt = at
				timer.Reset(d)
			}
		case <-timer.C:
			var files []file
			var needRetry bool
			for wavPath, fState := range recentChanged {
				if verbose {
					log.Println(suppress.Renderln("传送文件候选校验:", wavPath))
				}
				if fState.Stay == maxStay {
					log.Println(warn.Renderln("  长时间未准备就绪，暂时放弃以下文件"))
					log.Println("    ", wavPath)
					delete(recentChanged, wavPath)
					continue
				}

				txtPath := changeExt(wavPath, ".txt")
				s1, e1 := os.Stat(wavPath)
				s2, e2 := os.Stat(txtPath)
				if e1 != nil || e2 != nil {
					// Whenever this issue is resolved, an Create/Write event will occur.
					// So we ignore it for now.
					if verbose {
						log.Println(suppress.Renderln("  *.wav 与 *.txt 不匹配，暂时忽略"))
					}
					delete(recentChanged, wavPath)
					continue
				}
				s1Mod := s1.ModTime()
				s2Mod := s2.ModTime()
				if setting.Delta > 0 {
					// Whenever this issue is resolved, an Write event will occur.
					// So we ignore it for now.
					if math.Abs(s1Mod.Sub(s2Mod).Seconds()) > setting.Delta {
						if verbose {
							log.Println(suppress.Renderln("  *.wav 与 *.txt 更新时间相差", setting.Delta, "秒以上，暂时忽略"))
						}
						delete(recentChanged, wavPath)
						continue
					}
				}
				hash, err := verifyAndCalcHash(wavPath, txtPath)
				if err != nil {
					if verbose {
						log.Println(suppress.Renderln("  文件尚未准备就绪，暂时搁置"))
						log.Println(suppress.Renderln("    原因:", err))
					}
					fState.Stay++
					recentChanged[wavPath] = fState
					d := 500 * time.Millisecond
					at := time.Now().Add(d)
					if at.After(timerAt) {
						timerAt = at
						timer.Reset(d)
					}
					needRetry = true
				}
				if st, found := recentSent[wavPath]; found && st.Hash == hash {
					if verbose {
						log.Println(suppress.Renderln("  此为最近发送的文件，为避免重复发送暂时忽略"))
					}
					delete(recentChanged, wavPath)
					continue
				}
				if verbose {
					log.Println(suppress.Renderln("此文件为规则检索对象"))
				}
				files = append(files, file{wavPath, hash, s1Mod, fState.Retry})
			}
			if needRetry || len(files) == 0 {
				continue
			}
			needRetry, err = processFiles(L, files, setting.Sort, recentChanged, recentSent)
			if err != nil {
				log.Println("处理文件时出错:", err)
			}
			if needRetry {
				d := 500 * time.Millisecond
				at := time.Now().Add(d)
				if at.After(timerAt) {
					timerAt = at
					timer.Reset(d)
				}
			}
		}
	}
}

func main() {
	var mono bool
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&mono, "m", false, "disable color")
	flag.BoolVar(&preventClear, "prevent-clear", false, "prevent clear screen on reload")
	flag.Parse()

	if mono {
		warn = dummyColorizer{}
		caption = dummyColorizer{}
		suppress = dummyColorizer{}
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Fatalln("无法获取exe文件路径。", err)
	}

	settingFile := flag.Arg(0)
	if settingFile == "" {
		settingFile = filepath.Join(filepath.Dir(exePath), "setting.txt")
	}
	if !filepath.IsAbs(settingFile) {
		p, err := filepath.Abs(settingFile)
		if err != nil {
			log.Fatalln("filepath.Abs 失败:", err)
		}
		settingFile = p
	}

	if err := os.Chdir(filepath.Dir(exePath)); err != nil {
		log.Fatalln("当前目录更改失败:", err)
	}

	settingWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalln("fsnotify.NewWatcher 失败:", err)
	}
	defer settingWatcher.Close()

	err = settingWatcher.Add(filepath.Dir(settingFile))
	if err != nil {
		log.Fatalln("配置文件夹监视失败:", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalln("fsnotify.NewWatcher 失败:", err)
	}
	defer watcher.Close()

	recentChanged := map[string]fileState{}
	recentSent := map[string]sentFileState{}
	for i := 0; ; i++ {
		log.Println(caption.Renderln("监视者"), version)
		if verbose {
			log.Println(warn.Renderln("启用冗余日志模式"))
		}
		log.Println(suppress.Renderln("  配置文件:"), settingFile)
		log.Println()
		err = process(watcher, settingWatcher, settingFile, recentChanged, recentSent, i)
		if err != nil {
			log.Println(err)
			log.Println("3秒后重试")
			time.Sleep(3 * time.Second)
		}
	}
}
