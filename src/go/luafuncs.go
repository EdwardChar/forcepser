package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/oov/audio/wave"
	"github.com/yuin/gluare"
	lua "github.com/yuin/gopher-lua"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func luaDebugPrint(L *lua.LState) int {
	log.Println(L.ToString(1))
	return 0
}

func luaDebugError(L *lua.LState) int {
	log.Println(warn.Renderln(L.ToString(1)))
	return 0
}

func luaDebugPrintVerbose(L *lua.LState) int {
	if verbose {
		log.Println(suppress.Renderln(L.ToString(1)))
	}
	return 0
}

func luaExecute(path string, text string) lua.LGFunction {
	return func(L *lua.LState) int {
		nargs := L.GetTop()
		if nargs == 0 {
			return 0
		}
		tempFile := filepath.Join(os.TempDir(), fmt.Sprintf("forcepser%d.wav", time.Now().UnixNano()))
		defer os.Remove(tempFile)
		replacer := strings.NewReplacer("%BEFORE%", path, "%AFTER%", tempFile)
		var cmds []string
		for i := 1; i < nargs+1; i++ {
			cmds = append(cmds, replacer.Replace(L.ToString(i)))
		}
		if err := exec.Command(cmds[0], cmds[1:]...).Run(); err != nil {
			L.RaiseError("外部命令执行失败: %v", err)
		}
		f, err := os.Open(tempFile)
		if err == nil {
			defer f.Close()
			f2, err := os.Create(path)
			if err != nil {
				L.RaiseError("无法打开文件 %s : %v", path, err)
			}
			defer f2.Close()
			_, err = io.Copy(f2, f)
			if err != nil {
				L.RaiseError("复制文件时出错: %v", err)
			}
		}
		return 0
	}
}

func luaReplaceEnv(ss *setting) lua.LGFunction {
	return func(L *lua.LState) int {
		path := L.ToString(1)
		path = ss.dirReplacer.Replace(path)
		L.Push(lua.LString(path))
		return 1
	}
}

func copyFile(dst, src string) error {
	sf, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("无法打开复制源文件 %s : %w", src, err)
	}
	defer sf.Close()
	df, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("无法打开复制目标文件 %s : %w", dst, err)
	}
	defer df.Close()
	_, err = io.Copy(df, sf)
	if err != nil {
		return fmt.Errorf("文件复制 %s -> %s 失败: %w", src, dst, err)
	}
	return nil
}

func changeExt(path, ext string) string {
	return path[:len(path)-len(filepath.Ext(path))] + ext
}

func enumMoveTargetFiles(wavpath string) ([]string, error) {
	d, err := os.Open(filepath.Dir(wavpath))
	if err != nil {
		return nil, err
	}
	defer d.Close()

	fis, err := d.Readdir(0)
	if err != nil {
		return nil, err
	}

	fname := changeExt(filepath.Base(wavpath), "")
	r := []string{}
	for _, fi := range fis {
		n := fi.Name()
		if changeExt(n, "") == fname && !fi.IsDir() {
			r = append(r, n)
		}
	}
	return r, nil
}

func retry(f func() error, max int) error {
	var err error
	for i := 0; i < max; i++ {
		if err = f(); err == nil {
			return nil
		}
		if verbose {
			log.Println(suppress.Sprintf("%d次尝试失败: %v", i+1, err))
		}
		time.Sleep(300 * time.Millisecond)
	}
	if verbose {
		log.Println(suppress.Renderln("放弃重试"))
	}
	return err
}

func delayRemove(files []string, delay float64) {
	time.Sleep(time.Duration(delay) * time.Second)
	for _, f := range files {
		err := retry(func() error { return os.Remove(f) }, 3)
		if err != nil {
			log.Println(warn.Sprintf("移动源文件 %s 删除失败: %v", f, err))
		}
		if verbose {
			log.Println(suppress.Renderln("文件删除:", f))
		}
	}
}

func findGoodFileName(candidate, dir string) (string, error) {
	ext := filepath.Ext(candidate)
	name := candidate[:len(candidate)-len(ext)]
	prefix := filepath.Join(dir, name)
	a := ""
	i := 1
	for i <= 100 {
		if !exists(prefix + a + ext) {
			if verbose {
				log.Println(suppress.Renderln("重命名方案:", name+a+ext))
			}
			return name + a + ext, nil
		}
		i++
		a = fmt.Sprintf(" (%d)", i)
	}
	return candidate, fmt.Errorf("名称与 %s 相似的文件太多", candidate)
}

func luaFindRule(ss *setting) lua.LGFunction {
	return func(L *lua.LState) int {
		path := L.ToString(1)
		rule, text, err := ss.Find(path)
		if err != nil {
			L.RaiseError("搜索匹配条件时出错: %v", err)
		}
		if rule == nil {
			return 0
		}
		if rule.DeleteText {
			textfile := changeExt(path, ".txt")
			err = retry(func() error { return os.Remove(textfile) }, 3)
			if err != nil {
				L.RaiseError("%s 无法删除: %v", textfile, err)
			}
			log.Println("  按照 deletetext 设定删除 txt")
		}
		files, err := enumMoveTargetFiles(path)
		if err != nil {
			L.RaiseError("文件枚举失败: %v", err)
		}
		if rule.FileMove == "move" || rule.FileMove == "copy" {
			destDir := rule.ExpandedDestDir()
			srcDir := filepath.Dir(path)
			if strings.Contains(rule.DestDir, "%PROJECTDIR%") && ss.projectDir == "" {
				proj, err := readGCMZDropsData()
				if err != nil || proj.GCMZAPIVer < 1 {
					L.RaiseError("找不到装有‘随意拖放’ v0.3.13 或更高版本的 AviUtl")
				}
				if proj.Width == 0 {
					L.RaiseError("`找不到AviUtl中正在编辑的工程文件")
				}
				L.RaiseError("AviUtl工程文件尚未保存，处理无法继续")
			}
			destfi, err := getFileInfo(destDir)
			if err != nil {
				L.RaiseError("%s目标文件夹 %s 信息获取失败: %v", rule.FileMove.Readable(), destDir, err)
			}
			srcfi, err := getFileInfo(srcDir)
			if err != nil {
				L.RaiseError("%s源文件夹 %s 信息获取失败: %v", rule.FileMove.Readable(), srcDir, err)
			}
			if !isSameFileInfo(destfi, srcfi) {
				deleteFiles := []string{}
				for _, f := range files {
					oldpath := filepath.Join(srcDir, f)
					newpath := filepath.Join(destDir, f)
					err = retry(func() error { return copyFile(newpath, oldpath) }, 3)
					if err != nil {
						L.RaiseError("文件复制失败: %v", err)
					}
					if verbose {
						log.Println(suppress.Renderln("文件复制", oldpath, "->", newpath))
					}
					if rule.FileMove == "move" {
						deleteFiles = append(deleteFiles, oldpath)
					}
				}
				if rule.MoveDelay > 0 {
					go delayRemove(deleteFiles, rule.MoveDelay)
				} else {
					delayRemove(deleteFiles, 0)
				}
				log.Printf("  按照 filemove = \"%s\" 设定，将文件放到以下 %s 位置\n", rule.FileMove, rule.FileMove.Readable())
				log.Println("    ", destDir)
				path = filepath.Join(destDir, filepath.Base(path))
			}
		}
		layer := rule.Layer
		padding := lua.LValue(lua.LNumber(rule.Padding))
		userdata := lua.LValue(lua.LString(rule.UserData))
		exofile := lua.LValue(lua.LString(rule.ExoFile))
		luafile := lua.LValue(lua.LString(rule.LuaFile))
		if rule.Modifier != "" {
			L2 := lua.NewState()
			defer L2.Close()
			L2.PreloadModule("re", gluare.Loader)
			if err = L2.DoString(`re = require("re")`); err != nil {
				L.RaiseError("modifier脚本初始化时出错: %v", err)
			}
			L2.SetGlobal("debug_print", L2.NewFunction(luaDebugPrint))
			L2.SetGlobal("debug_error", L2.NewFunction(luaDebugError))
			L2.SetGlobal("debug_print_verbose", L2.NewFunction(luaDebugPrintVerbose))
			L2.SetGlobal("getaudioinfo", L2.NewFunction(luaGetAudioInfo))
			L2.SetGlobal("execute", L2.NewFunction(luaExecute(path, text)))
			L2.SetGlobal("tofilename", L2.NewFunction(luaToFilename))
			L2.SetGlobal("layer", lua.LNumber(layer))
			L2.SetGlobal("text", lua.LString(text))
			filename := filepath.Base(path)
			L2.SetGlobal("filename", lua.LString(filename))
			L2.SetGlobal("wave", lua.LString(path))
			L2.SetGlobal("padding", padding)
			L2.SetGlobal("userdata", userdata)
			L2.SetGlobal("exofile", exofile)
			L2.SetGlobal("luafile", luafile)
			if err = L2.DoString(rule.Modifier); err != nil {
				L.RaiseError("运行modifier脚本时出错: %v", err)
			}
			layer = int(lua.LVAsNumber(L2.GetGlobal("layer")))
			text = L2.GetGlobal("text").String()
			padding = L2.GetGlobal("padding")
			userdata = L2.GetGlobal("userdata")
			exofile = L2.GetGlobal("exofile")
			luafile = L2.GetGlobal("luafile")

			if newfilename := L2.GetGlobal("filename").String(); filename != newfilename {
				dir := filepath.Dir(path)
				newfilename, err = findGoodFileName(newfilename, dir)
				if err != nil {
					L.RaiseError("找不到文件名候选: %v", err)
				}
				for _, f := range files {
					oldpath := filepath.Join(dir, f)
					newpath := filepath.Join(dir, changeExt(newfilename, filepath.Ext(f)))
					err = retry(func() error { return os.Rename(oldpath, newpath) }, 3)
					if err != nil {
						L.RaiseError("文件重命名失败: %v", err)
					}
					if verbose {
						log.Println(suppress.Renderln("重命名:", oldpath, "->", newpath))
					}
				}
				path = filepath.Join(dir, newfilename)
			}
		}

		t := L.NewTable()
		t.RawSetString("dir", lua.LString(rule.Dir))
		t.RawSetString("file", lua.LString(rule.File))
		t.RawSetString("encoding", lua.LString(rule.Encoding))
		t.RawSetString("layer", lua.LNumber(layer))
		t.RawSetString("text", lua.LString(rule.Text))
		t.RawSetString("userdata", userdata)
		t.RawSetString("padding", padding)
		t.RawSetString("exofile", exofile)
		t.RawSetString("luafile", luafile)
		L.Push(t)
		L.Push(lua.LString(text))
		L.Push(lua.LString(path))
		return 3
	}
}

func luaGetAudioInfo(L *lua.LState) int {
	f, err := os.Open(L.ToString(1))
	if err != nil {
		L.RaiseError("无法打开文件: %v", err)
	}
	defer f.Close()
	r, wfe, err := wave.NewLimitedReader(f)
	if err != nil {
		L.RaiseError("Wave文件读取失败: %v", err)
	}
	t := L.NewTable()
	t.RawSetString("samplerate", lua.LNumber(wfe.Format.SamplesPerSec))
	t.RawSetString("channels", lua.LNumber(wfe.Format.Channels))
	t.RawSetString("bits", lua.LNumber(wfe.Format.BitsPerSample))
	t.RawSetString("samples", lua.LNumber(r.N/int64(wfe.Format.Channels)/int64(wfe.Format.BitsPerSample/8)))
	L.Push(t)
	return 1
}

func luaFromGBK(L *lua.LState) int {
	s, err := simplifiedchinese.GBK.NewDecoder().String(L.ToString(1))
	if err != nil {
		L.RaiseError("无法从 GBK 转换为字符串: %v", err)
	}
	L.Push(lua.LString(s))
	return 1
}

func luaToGBK(L *lua.LState) int {
	s, err := simplifiedchinese.GBK.NewEncoder().String(L.ToString(1))
	if err != nil {
		L.RaiseError("无法将字符串转换为 GBK: %v", err)
	}
	L.Push(lua.LString(s))
	return 1
}

func luaToFilename(L *lua.LState) int {
	var nc int
	var rs []rune
	n := int(L.ToNumber(2))
	for _, c := range L.ToString(1) {
		switch c {
		case
			0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
			0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
			0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
			0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
			0x20, 0x22, 0x2a, 0x2f, 0x3a, 0x3c, 0x3e, 0x3f, 0x7c, 0x7f:
			continue
		}
		nc++
		if nc == n+1 {
			rs[len(rs)-1] = '…'
			break
		}
		rs = append(rs, c)
	}
	L.Push(lua.LString(string(rs)))
	return 1
}

var hexChars = [16]byte{'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', 'a', 'b', 'c', 'd', 'e', 'f'}

func luaToEXOString(L *lua.LState) int {
	u16 := utf16.Encode([]rune(L.ToString(1)))
	buf := make([]byte, 1024*4)
	for i, c := range u16 {
		buf[i*4+0] = hexChars[(c>>4)&15]
		buf[i*4+1] = hexChars[(c>>0)&15]
		buf[i*4+2] = hexChars[(c>>12)&15]
		buf[i*4+3] = hexChars[(c>>8)&15]
	}
	for i := len(u16) * 4; i < len(buf); i++ {
		buf[i] = '0'
	}
	L.Push(lua.LString(buf))
	return 1
}

func atoich(a byte) int {
	if '0' <= a && a <= '9' {
		return int(a - '0')
	}
	return int(a&0xdf - 'A' + 10)
}

func luaFromEXOString(L *lua.LState) int {
	src := L.ToString(1)
	var buf [1024]rune
	for i := 0; i+3 < len(src); i += 4 {
		buf[i/4] = rune((atoich(src[i]) << 4) | atoich(src[i+1]) | (atoich(src[i+2]) << 12) | (atoich(src[i+3]) << 8))
		if buf[i/4] == 0 {
			L.Push(lua.LString(buf[:i/4]))
			return 1
		}
	}
	L.Push(lua.LString(buf[:]))
	return 1
}
