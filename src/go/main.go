package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/yuin/gluare"
	lua "github.com/yuin/gopher-lua"
)

var verbose bool

type file struct {
	Filepath string
	ModDate  time.Time
	TryCount int
}

func processFiles(L *lua.LState, files []file, recentChanged map[string]int, recentSent map[string]time.Time) (needRetry bool, err error) {
	defer func() {
		for k, ct := range recentChanged {
			if ct == 9 {
				log.Println("  たくさん失敗したのでこのファイルは諦めます:", k)
				delete(recentChanged, k)
				continue
			}
			recentChanged[k] = ct + 1
			needRetry = true
		}
	}()
	proj, err := readGCMZDropsData()
	if err != nil {
		if verbose {
			log.Println("[INFO] プロジェクト情報取得失敗:", err)
		}
		err = fmt.Errorf("ごちゃまぜドロップス v0.3 以降がインストールされた AviUtl が検出できませんでした")
		return
	}
	if proj.Width == 0 {
		err = fmt.Errorf("AviUtl で編集中のプロジェクトが見つかりません")
		return
	}
	if verbose {
		log.Println("[INFO] プロジェクト情報:")
		if proj.GCMZAPIVer >= 1 {
			log.Println("[INFO]   ProjectFile:", proj.ProjectFile)
		}
		log.Println("[INFO]   Window:", int(proj.Window))
		log.Println("[INFO]   Width:", proj.Width)
		log.Println("[INFO]   Height:", proj.Height)
		log.Println("[INFO]   VideoRate:", proj.VideoRate)
		log.Println("[INFO]   VideoScale:", proj.VideoScale)
		log.Println("[INFO]   AudioRate:", proj.AudioRate)
		log.Println("[INFO]   AudioCh:", proj.AudioCh)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].ModDate.Before(files[j].ModDate)
	})
	t := L.NewTable()
	tc := L.NewTable()
	for _, f := range files {
		t.Append(lua.LString(f.Filepath))
		tc.Append(lua.LNumber(f.TryCount))
	}
	pt := L.NewTable()
	pt.RawSetString("projectfile", lua.LString(proj.ProjectFile))
	pt.RawSetString("gcmzapiver", lua.LNumber(proj.GCMZAPIVer))
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
	}, t, tc, pt); err != nil {
		return
	}
	rv := L.ToTable(-1)
	if rv == nil {
		err = fmt.Errorf("処理後の戻り値が異常です")
		return
	}
	// remove processed entries
	now := time.Now()
	n := rv.MaxN()
	for i := 1; i <= n; i++ {
		k := rv.RawGetInt(i).String()
		recentSent[k] = now
		delete(recentChanged, k)
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
					log.Println("[INFO]", "  AviUtl のプロジェクトパスの変更を検出しました")
				}
				notify <- nil
				return
			}
		}
	}
}

func watch(ctx context.Context, watcher *fsnotify.Watcher, notify chan<- map[string]struct{}, settingFile string, freshness float64) {
	defer close(notify)
	var finish bool
	changed := map[string]struct{}{}
	timer := time.NewTimer(100 * time.Millisecond)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-watcher.Events:
			if verbose {
				log.Println("[INFO]", "イベント検証:", event)
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				if verbose {
					log.Println("[INFO]", "  オペレーションが Create / Write ではないので何もしません")
				}
				continue
			}
			if event.Name == settingFile {
				if verbose {
					log.Println("[INFO]", "  設定ファイルの再読み込みとして処理します")
				}
				finish = true
				timer.Reset(100 * time.Millisecond)
				continue
			}
			ext := strings.ToLower(filepath.Ext(event.Name))
			if ext != ".wav" && ext != ".txt" {
				if verbose {
					log.Println("[INFO]", "  *.wav / *.txt のどちらでもないので何もしません")
				}
				continue
			}
			if freshness > 0 {
				st, err := os.Stat(event.Name)
				if err != nil {
					if verbose {
						log.Println("[INFO]", "  更新日時の取得に失敗したので何もしません")
					}
					continue
				}
				if math.Abs(time.Now().Sub(st.ModTime()).Seconds()) > freshness {
					if verbose {
						log.Println("[INFO]", "  更新日時が", freshness, "秒以上前なので何もしません")
					}
					continue
				}
			}
			if verbose {
				log.Println("[INFO]", "  送信ファイル候補にします")
			}
			changed[event.Name[:len(event.Name)-len(ext)]+".wav"] = struct{}{}
			timer.Reset(100 * time.Millisecond)
		case err := <-watcher.Errors:
			log.Println("監視中にエラーが発生しました:", err)
		case <-timer.C:
			if finish {
				notify <- nil
				break
			}
			notify <- changed
			changed = map[string]struct{}{}
		}
	}
}

func process(watcher *fsnotify.Watcher, settingFile string, recentChanged map[string]int, recentSent map[string]time.Time, timer *time.Timer, loop int) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("exe ファイルのパスが取得できません: %w", err)
	}
	tempDir := filepath.Join(filepath.Dir(exePath), "tmp")

	projectPath := getProjectPath()
	var projectDir string
	if projectPath != "" {
		projectDir = filepath.Dir(projectPath)
	}

	setting, err := newSetting(settingFile, tempDir, projectDir)
	if err != nil {
		return fmt.Errorf("設定の読み込みに失敗しました: %w", err)
	}

	log.Println("  環境変数:")
	log.Println("    %BASEDIR%:", setting.BaseDir)
	log.Println("    %TEMPDIR%:", tempDir)
	log.Println("    %PROJECTDIR%:", setting.projectDir)
	log.Println("    %PROFILE%", getSpecialFolderPath(CSIDL_PROFILE))
	log.Println("    %DESKTOP%", getSpecialFolderPath(CSIDL_DESKTOP))
	log.Println("    %MYDOC%", getSpecialFolderPath(CSIDL_PERSONAL))
	log.Println()

	L := lua.NewState()
	defer L.Close()

	L.PreloadModule("re", gluare.Loader)
	err = L.DoString(`re = require("re")`)
	if err != nil {
		return fmt.Errorf("Lua スクリプト環境の初期化中にエラーが発生しました: %w", err)
	}

	L.SetGlobal("debug_print", L.NewFunction(luaDebugPrint))
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
		return fmt.Errorf("_entrypoint.lua の実行中にエラーが発生しました: %w", err)
	}

	log.Println("  delta:", setting.Delta)
	log.Println("  freshness:", setting.Freshness)
	log.Println()

	updateOnly := loop > 0
	for i, a := range setting.Asas {
		log.Printf("  Asas %d:", i+1)
		log.Println("    対象EXE:", a.Exe)
		log.Println("    フィルター:", a.Filter)
		log.Println("    保存先フォルダー:", a.Folder)
		log.Println("    フォーマット:", a.Format)
		log.Println("    フラグ:", a.Flags)
		if a.Exists() {
			if _, err := a.ConfirmAndRun(updateOnly); err != nil {
				return fmt.Errorf("プログラムの起動に失敗しました: %w", err)
			}
		} else {
			log.Println("    [警告] 対象EXE が見つからないため設定を無視します")
		}
	}
	log.Println()
	for i, r := range setting.Rule {
		log.Printf("  ルール%d:", i+1)
		log.Println("    対象フォルダー:", r.Dir)
		log.Println("    対象ファイル名:", r.File)
		log.Println("    テキストファイルの文字コード:", r.Encoding)
		if r.textRE != nil {
			log.Println("    テキスト判定用の正規表現:", r.Text)
		}
		log.Println("    挿入先レイヤー:", r.Layer)
		if r.Modifier != "" {
			log.Println("    modifier: あり")
		} else {
			log.Println("    modifier: なし")
		}
		log.Println("    ユーザーデータ:", r.UserData)
		log.Println("    パディング:", r.Padding)
		log.Println("    EXOファイル:", r.ExoFile)
		log.Println("    Luaファイル:", r.LuaFile)
		switch r.FileMove {
		case "off":
			log.Println("    Waveファイルの移動: しない")
		case "move":
			log.Println("    Waveファイルの移動: *.aup と同じ場所に移動")
		case "copy":
			log.Println("    Waveファイルの移動: *.aup と同じ場所にコピー")
		}
		if r.DeleteText {
			log.Println("    テキストファイルの削除: する")
		} else {
			log.Println("    テキストファイルの削除: しない")
		}
		if !r.ExistsDir() {
			log.Println("    [警告] 対象フォルダー が見つからないため設定を無視します")
		}
	}
	log.Println()

	if err = os.Mkdir(tempDir, 0777); err != nil && !os.IsExist(err) {
		return fmt.Errorf("tmp フォルダの作成に失敗しました: %w", err)
	}

	log.Println("監視を開始します:")
	watching := 0
	for _, dir := range setting.Dirs() {
		err = watcher.Add(dir)
		if err != nil {
			return fmt.Errorf("フォルダー %q が監視できません: %w", dir, err)
		}
		log.Println("  " + dir)
		watching++
		defer watcher.Remove(dir)
	}
	if watching == 0 {
		log.Println("  [警告] 監視対象のフォルダーがひとつもありません")
	}
	notify := make(chan map[string]struct{}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchProjectPath(ctx, notify, projectPath)
	go watch(ctx, watcher, notify, settingFile, setting.Freshness)
	for files := range notify {
		if files == nil {
			log.Println()
			log.Println("設定ファイルを再読み込みします")
			log.Println()
			return nil
		}

		now := time.Now()
		for k := range recentSent {
			if now.Sub(recentSent[k]) > 5*time.Second {
				delete(recentSent, k)
			}
		}
		for k := range files {
			if _, ok := recentChanged[k]; !ok {
				recentChanged[k] = 0
			}
		}
		var files []file
		for k, tryCount := range recentChanged {
			if verbose {
				log.Println("[INFO]", "送信ファイル候補検証:", k)
			}
			if _, found := recentSent[k]; found {
				if verbose {
					log.Println("[INFO]", "  つい最近送ったファイルなので、重複送信回避のために無視します")
				}
				delete(recentChanged, k)
				continue
			}
			s1, e1 := os.Stat(k)
			s2, e2 := os.Stat(k[:len(k)-4] + ".txt")
			if e1 != nil || e2 != nil {
				if verbose {
					log.Println("[INFO]", "  *.wav と *.txt が揃ってないので無視します")
				}
				delete(recentChanged, k)
				continue
			}
			s1Mod := s1.ModTime()
			s2Mod := s2.ModTime()
			if setting.Delta > 0 {
				if math.Abs(s1Mod.Sub(s2Mod).Seconds()) > setting.Delta {
					if verbose {
						log.Println("[INFO]", "  *.wav と *.txt の更新日時の差が", setting.Delta, "秒以上なので無視します")
					}
					delete(recentChanged, k)
					continue
				}
			}
			if verbose {
				log.Println("[INFO]", "  このファイルはルール検索対象です")
			}
			files = append(files, file{k, s1Mod, tryCount})
		}
		if len(files) == 0 {
			continue
		}
		needRetry, err := processFiles(L, files, recentChanged, recentSent)
		if err != nil {
			log.Println("ファイルの処理中にエラーが発生しました:", err)
		}
		if needRetry {
			timer.Reset(500 * time.Millisecond)
		}
	}
	return nil
}

func main() {
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.Parse()
	log.Println("かんしくん", version)
	if verbose {
		log.Println("  [INFO] 冗長ログモードが有効です")
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Fatalln("exe ファイルのパスが取得できません", err)
	}

	settingFile := flag.Arg(0)
	if settingFile == "" {
		settingFile = filepath.Join(filepath.Dir(exePath), "setting.txt")
	}
	if !filepath.IsAbs(settingFile) {
		p, err := filepath.Abs(settingFile)
		if err != nil {
			log.Fatalln("filepath.Abs に失敗しました:", err)
		}
		settingFile = p
	}

	if err := os.Chdir(filepath.Dir(exePath)); err != nil {
		log.Fatalln("カレントディレクトリの変更に失敗しました:", err)
	}

	log.Println("設定ファイル:")
	log.Println("  " + settingFile)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalln("fsnotify.NewWatcher に失敗しました:", err)
	}
	defer watcher.Close()

	err = watcher.Add(settingFile)
	if err != nil {
		log.Fatalln("設定ファイルの監視に失敗しました:", err)
	}

	recentChanged := map[string]int{}
	recentSent := map[string]time.Time{}
	timer := time.NewTimer(100 * time.Millisecond)
	timer.Stop()
	for i := 0; ; i++ {
		err = process(watcher, settingFile, recentChanged, recentSent, timer, i)
		if err != nil {
			log.Println(err)
			log.Println("3秒後にリトライします")
			time.Sleep(3 * time.Second)
		}
	}
}
