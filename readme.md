# simple-updater 使用教程

simple-updater 是一个面向 Windows 和 macOS 的 Go 更新组件，负责：

- 识别并解析 Windows Inno Setup 安装包和 macOS DMG；
- 将安装包、文件清单和文件对象上传到阿里云 OSS，并把版本元数据保存到 PostgreSQL；
- 查询最新版本、比较本地文件、生成 gzip 压缩的 tar 差分包；
- 通过独立的 updater helper 替换正在运行的应用文件，并在成功后重启应用。

它不是 HTTP 服务，也不包含版本发布后台。OSS 和 PostgreSQL 需要由调用方提供。

本文档对应当前仓库 v0.2.0（HEAD 6190caf，Add streaming archive updates）。

## 1. 安装

项目要求 Go 1.26.5 或更高版本。应用项目中执行：

~~~bash
go get github.com/Peiratooo/simple-updater@v0.2.0
~~~

也可以使用仓库最新标签：

~~~bash
go get github.com/Peiratooo/simple-updater@latest
~~~

导入：

~~~go
import simpleupdater "github.com/Peiratooo/simple-updater"
~~~

构建 updater helper 时使用仓库内的子包：

~~~go
import "github.com/Peiratooo/simple-updater/updaterstub"
~~~

## 2. 核心概念

### 2.1 支持的平台和安装包

| 输入 | PackageType | System | 解析入口 |
| --- | --- | --- | --- |
| Inno Setup EXE | PackageTypeInno ("inno") | windows | AnalyzeInnoSetupEXE |
| macOS DMG | PackageTypeDMG ("dmg") | darwin | AnalyzeSetupDMG |

AnalyzePackage 通过 PE/MZ 或 DMG koly 标识判断类型。识别为 Inno 类型后，仍由 Inno 解析器验证并提取内容。

### 2.2 SetupReader

安装包参数不要求具体的 *os.File 类型，只要实现以下接口即可：

~~~go
type SetupReader interface {
	io.Reader
	io.ReaderAt
	io.Seeker
}
~~~

因此 *os.File、*bytes.Reader 等都可以直接传入分析和发布函数。

### 2.3 File 和 manifest

文件清单使用 []File 表示。manifest.json 是一个 File 数组，更新 payload 必须与其中的 Path 保持相同的相对路径。

~~~json
[
  {
    "path": "Demo.exe",
    "type": "file",
    "size": 123456,
    "sha256": "0123456789abcdef...",
    "mode": 493,
    "url": "releases/1.2.0-windows-uuid/Demo.exe"
  },
  {
    "path": "Contents/Resources/current",
    "type": "symlink",
    "mode": 511,
    "link_target": "1.2.0"
  }
]
~~~

字段用途：

| 字段 | 用途 |
| --- | --- |
| Path | 相对安装根目录的文件路径，必须使用安全的相对路径 |
| Type | FileTypeRegular ("file") 或 FileTypeSymlink ("symlink")；空值按普通文件处理 |
| Size | 普通文件的字节数 |
| SHA256 | 普通文件的 SHA-256，用于补丁前后校验 |
| Mode | 文件权限，例如 0755 对应十进制 493；未设置时使用默认权限 |
| LinkTarget | 符号链接目标；只对 symlink 有效 |
| URL | OSS 对象 key；下载差分文件时使用 |
| Data | 运行时文件内容，不写入 JSON 或数据库 |

路径穿越、绝对路径、重复路径以及越出根目录的符号链接会被拒绝；.simple-updater-state 是 updater 保留目录，不能放进 manifest。

## 3. 构建 updater helper

应用发布前需要准备一个 helper。它在目标机器上执行替换和重启，最终用户不需要安装 Go。

### 3.1 命令行构建

在仓库根目录执行：

~~~bash
go run ./cmd/build-updaters
~~~

默认输出：

~~~text
dist/updater-windows-amd64.exe
dist/updater-darwin-universal
~~~

### 3.2 在 Go 代码中构建

~~~go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Peiratooo/simple-updater/updaterstub"
)

func main() {
	windowsPath, err := updaterstub.Build(context.Background(), updaterstub.BuildOptions{
		System: "windows",
		Arch:   "amd64",
		Output: "dist/updater-windows-amd64.exe",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(windowsPath)

	darwinPath, err := updaterstub.BuildUniversalDarwin(context.Background(), updaterstub.UniversalBuildOptions{
		Output: "dist/updater-darwin-universal",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(darwinPath)
}
~~~

Build 支持 windows/darwin，架构支持 amd64/arm64；mac 和 macos 是系统别名。BuildUniversalDarwin 会在 Go 中合并 amd64 和 arm64，不需要 lipo 或 Xcode。

发布时把对应 helper 放进应用安装目录之外或应用约定的位置，并把路径传给 StartUpdater / ExecuteUpdate。不要在更新 payload 目录中放 helper。

## 4. 发布安装包到 OSS 和 PostgreSQL

Client 同时嵌入 OSS 和 DB 配置：

~~~go
package main

import (
	"fmt"
	"log"
	"os"

	simpleupdater "github.com/Peiratooo/simple-updater"
)

func main() {
	client := simpleupdater.New(&simpleupdater.Client{
		OSS: simpleupdater.OSS{
			ID:       os.Getenv("OSS_ACCESS_KEY_ID"),
			Key:      os.Getenv("OSS_ACCESS_KEY_SECRET"),
			Endpoint: os.Getenv("OSS_ENDPOINT"),
			Bucket:   os.Getenv("OSS_BUCKET"),
			Folder:   "releases",
		},
		DB: simpleupdater.DB{
			Host:     os.Getenv("PGHOST"),
			Port:     "5432",
			Username: os.Getenv("PGUSER"),
			Password: os.Getenv("PGPASSWORD"),
			Database: os.Getenv("PGDATABASE"),
			// Schema 实际作为 GORM 的表名传入，不是 PostgreSQL schema 名。
			Schema: "products",
		},
	})

	setup, err := os.Open("DemoSetup.exe")
	if err != nil {
		log.Fatal(err)
	}
	defer setup.Close()

	product, err := client.Push(setup)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("published %s %s: %s (%d bytes)\n",
		product.Product, product.Version, product.URL, product.Size)
}
~~~

New 会连接 OSS 并初始化 PostgreSQL 表；当前实现初始化失败时会调用 log.Fatal 终止进程，因此生产代码应在调用前检查配置。Push 的流程是：分析安装包 → 提取产品信息和文件 → 计算大小及 SHA-256 → 生成文件名 → 上传安装包和文件 → 写入 Product 元数据。

生成的安装包文件名类似：

~~~text
Demo-App-1.2.0-windows-setup.exe
Demo-App-1.2.0-darwin-setup.dmg
~~~

## 5. 查询版本、比较文件并生成差分包

客户端通常先扫描本地安装目录，再用 Compare 获取最新版本中新增或变化的文件：

~~~go
package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"

	simpleupdater "github.com/Peiratooo/simple-updater"
)

func update(client *simpleupdater.Client, installRoot string) {
	localFiles, err := simpleupdater.ReadProductManifest(installRoot)
	if err != nil {
		log.Fatal(err)
	}

	changed, err := client.Compare("windows", "com.example.demo", localFiles)
	if err != nil {
		log.Fatal(err)
	}
	if len(changed) == 0 {
		return
	}

	archive, err := client.DownloadPatch(changed)
	if err != nil {
		log.Fatal(err)
	}

	pid, err := simpleupdater.StartUpdater(simpleupdater.UpdaterLaunchOptions{
		UpdaterPath: "updater-windows-amd64.exe",
		PID:         os.Getpid(),
		InstallRoot: installRoot,
		RestartPath: filepath.Join(installRoot, "Demo.exe"),
		Archive:     bytes.NewReader(archive),
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("updater started: %d", pid)

	// StartUpdater 成功后 helper 已经拿到完整脚本；当前应用应立即退出，
	// 让 helper 替换正在使用的文件。
	os.Exit(0)
}
~~~

注意：Compare 只返回最新版本中的新增/变更文件，不会返回“旧版本有但新版本已删除”的文件。DownloadPatch 会：

1. 下载每个普通文件的 OSS 对象；
2. 校验大小和 SHA-256；
3. 写入 manifest.json；
4. 返回 gzip 压缩的 tar 数据。

符号链接不会从 OSS 下载内容，而是以 tar symlink 条目写入补丁。

如果只想下载完整安装包：

~~~go
product, err := client.DownloadLatestSetup("windows", "com.example.demo")
if err != nil {
	log.Fatal(err)
}
// product.Bytes 是安装包内容；product.URL 是 OSS object key。
~~~

DownloadLatestSetup 只负责下载，不会直接安装。只查询元数据时使用 GetLatestSetupInfo。

按发布时间从旧到新获取全部未删除版本：

~~~go
versions, err := client.GetAllSetupInfo("windows", "com.example.demo")
if err != nil {
	log.Fatal(err)
}
for _, version := range versions {
	fmt.Println(version.Version, version.CreatedTime)
}
~~~

没有匹配版本时返回空切片和 nil error。

## 6. 直接准备目录并执行更新

不使用 DownloadPatch 时，也可以自己准备一个更新目录。目录必须包含 manifest.json 和对应的 payload 文件：

~~~text
update-root/
├── manifest.json
├── Demo.exe
└── resources/
    └── app.json
~~~

一种简单的发布端生成方式是先扫描 payload，再写清单：

~~~go
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	simpleupdater "github.com/Peiratooo/simple-updater"
)

func main() {
	updateRoot := "new-build"
	files, err := simpleupdater.ReadProductManifest(updateRoot)
	if err != nil {
		log.Fatal(err)
	}

	manifest, err := json.Marshal(files)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(updateRoot, "manifest.json"), manifest, 0o644); err != nil {
		log.Fatal(err)
	}

	// 成功交接后 ExecuteUpdate 会 os.Exit(0)，不会继续执行后面的代码。
	if err := simpleupdater.ExecuteUpdate("updater-windows-amd64.exe", updateRoot); err != nil {
		log.Fatal(err)
	}
}
~~~

ExecuteUpdate 会自动根据当前进程定位安装根目录：

- Windows：当前可执行文件所在目录，并重启当前可执行文件；
- macOS：当前可执行文件所属的 .app 根目录，并重新打开该 .app。

它只支持 Windows 和 macOS。成功交接后当前进程会退出；只有准备或启动 helper 失败时才会返回 error。

### 6.1 使用 StartUpdater 的两种模式

归档模式最简单：

~~~go
pid, err := simpleupdater.StartUpdater(simpleupdater.UpdaterLaunchOptions{
	UpdaterPath: "updater.exe",
	PID:         os.Getpid(),
	InstallRoot: installRoot,
	RestartPath: restartPath,
	Archive:     bytes.NewReader(gzipTarArchive),
})
~~~

也可以手动指定已经存在的补丁目录和生成好的脚本：

~~~go
script, err := simpleupdater.GenerateUpdateScript("windows", files)
if err != nil {
	return err
}

_, err = simpleupdater.StartUpdater(simpleupdater.UpdaterLaunchOptions{
	UpdaterPath: "updater.exe",
	PID:         os.Getpid(),
	InstallRoot: installRoot,
	PatchRoot:   updateRoot,
	RestartPath: restartPath,
	Script:      []byte(script),
})
~~~

Archive 模式下 PatchRoot 和 Script 必须为空；普通模式下必须同时提供 PatchRoot 和非空 Script。StartUpdater 返回 helper PID 后，调用方负责退出当前应用。

## 7. DMG 解析示例

通常直接调用 AnalyzeSetupDMG 即可；需要访问 .app 文件系统时使用底层 API：

~~~go
setup, err := os.Open("Demo.dmg")
if err != nil {
	return err
}
defer setup.Close()

app, cleanup, err := simpleupdater.ExtractDMGApp(setup)
if err != nil {
	return err
}
defer cleanup()

info, err := simpleupdater.ReadInfoPlist(app)
if err != nil {
	return err
}
files, err := simpleupdater.ScanRoot(app)
if err != nil {
	return err
}

fmt.Println(info.AppID, info.AppName, info.AppVersion, len(files))
~~~

ReadInfoPlist 读取 Contents/Info.plist，并要求 CFBundleIdentifier、CFBundleVersion、CFBundleName 都非空。ExtractDMGApp 返回的 cleanup 必须调用；AnalyzeSetupDMG 已经替调用方完成了这一步。

## 8. 暴露的 API

以下是根包 github.com/Peiratooo/simple-updater 当前所有对外函数和方法。

### 8.1 客户端和存储

| API | 目的 |
| --- | --- |
| New(client *Client) *Client | 初始化 OSS 客户端、连接 PostgreSQL，并自动迁移 Product 表 |
| (*Client).Push(setup SetupReader) (*Product, error) | 分析并上传 EXE/DMG，同时写入产品元数据 |
| (*Client).Compare(system, appID string, files []File) ([]File, error) | 查询最新未删除版本，返回本地缺失或已变化的文件 |
| (*Client).GetLatestSetupInfo(system, appID string) (*Product, error) | 只读取最新安装包的数据库元数据 |
| (*Client).GetAllSetupInfo(system, appID string) ([]Product, error) | 按创建时间从旧到新返回全部未删除版本 |
| (*Client).DownloadLatestSetup(system, appID string) (*Product, error) | 读取最新元数据，并把安装包内容放到 Product.Bytes |
| (*Client).DownloadFile(path string) ([]byte, error) | 按 OSS object key 下载一个对象 |
| (*Client).DownloadPatch(files []File) ([]byte, error) | 下载指定文件并生成 gzip+tar 差分包 |

### 8.2 安装包分析和工具函数

| API | 目的 |
| --- | --- |
| AnalyzePackage(file SetupReader) (string, PackageType, error) | 识别 windows/inno 或 darwin/dmg |
| AnalyzeSystem(file SetupReader) (string, error) | 只返回平台名称，是 AnalyzePackage 的简化封装 |
| AnalyzeInnoSetupEXE(setup SetupReader) (*Product, error) | 解析 Inno Setup EXE，提取产品信息和安装文件 |
| AnalyzeSetupDMG(setup SetupReader) (*Product, error) | 找到唯一 .app，读取 plist 并扫描文件 |
| GenerateSize(file io.Seeker) (int64, error) | 获取文件大小，并恢复调用前的 seek 位置 |
| GenerateSHA256(file SetupReader) (string, error) | 计算整个输入的 SHA-256，并保持原 seek 位置 |
| GenerateSetupFileName(product *Product) (string, error) | 根据产品名、版本、系统和包类型生成安全文件名 |
| ReadProductManifest(root string) ([]File, error) | 递归扫描本地目录，生成普通文件/符号链接清单 |
| ScanRoot(root fs.FS) ([]File, error) | 递归扫描任意 fs.FS，读取内容并计算普通文件 SHA-256 |

ReadProductManifest 的名称容易产生误解：它扫描的是目录，并不读取已有的 manifest.json。

### 8.3 DMG 和更新脚本

| API | 目的 |
| --- | --- |
| ExtractDMGApp(setup SetupReader) (fs.FS, func(), error) | 解压/挂载 DMG 内的 APFS 或 HFS+，返回唯一 .app 文件系统和 cleanup 函数 |
| ReadInfoPlist(app fs.FS) (AppInfo, error) | 读取 .app/Contents/Info.plist 的应用标识、名称和版本 |
| GenerateUpdateScript(system string, manifest []File) (string, error) | 生成 Windows PowerShell 或 macOS /bin/sh 更新脚本 |
| GenerateUpdateScriptFromJSON(system string, manifest []byte) (string, error) | 解析 JSON 清单后生成平台更新脚本 |
| StartUpdater(options UpdaterLaunchOptions) (int, error) | 复制 helper 到系统临时目录、传入脚本并脱离启动 |
| ExecuteUpdate(updaterPath, updateRoot string) error | 从 updateRoot 读取 manifest，启动 helper，并在成功交接后退出当前进程 |

脚本本身不会从 GenerateUpdateScript 返回后自动执行；必须通过 StartUpdater 或自行交给 helper 执行。

### 8.4 类型、常量和方法

~~~go
type PackageType string

const (
	PackageTypeInno PackageType = "inno"
	PackageTypeDMG  PackageType = "dmg"
)

func (p PackageType) System() string     // inno -> windows, dmg -> darwin
func (p PackageType) Extension() string  // inno -> .exe, dmg -> .dmg

type FileType string

const (
	FileTypeRegular FileType = "file"
	FileTypeSymlink FileType = "symlink"
)
~~~

主要结构体：

- AppInfo：AppID、AppVersion、AppName，对应 macOS plist 字段；
- Product：产品名、系统、包类型、版本、SHA-256、OSS URL、大小、文件清单和 UUID；Bytes 用于下载内容，Data 用于上传输入，两者都不会进入 JSON/数据库；
- File：更新文件的路径、类型、大小、哈希、权限、符号链接目标和 OSS URL；
- Client：嵌入 OSS 和 DB；
- OSS：ID、Key、Endpoint、Bucket、Folder 以及初始化后的 Client；
- DB：Host、Port、Username、Password、Database、Schema 以及初始化后的 Engine；
- UpdaterLaunchOptions：helper 路径、旧进程 PID、安装根、补丁根、重启路径、脚本或归档输入。

### 8.5 updaterstub 子包

包路径：github.com/Peiratooo/simple-updater/updaterstub

| API | 目的 |
| --- | --- |
| Build(ctx, options BuildOptions) (string, error) | 交叉编译单个 Windows 或 macOS helper |
| BuildUniversalDarwin(ctx, options UniversalBuildOptions) (string, error) | 构建并合并 macOS amd64/arm64 Universal helper |

BuildOptions 字段：System、Arch、Output、Package、WorkDir。默认 Package 是 ./updaterstub/cmd/updater，默认架构是当前 Go 运行时架构。UniversalBuildOptions 提供 Output、Package、WorkDir。

## 9. 更新过程和运行约束

生成的脚本会执行以下步骤：

1. 等待旧应用退出；Windows 等待后强制结束，macOS 依次发送 TERM/KILL；
2. 预检查目标目录可写、目标文件未锁定、补丁文件大小和 SHA-256；
3. 修改前备份所有目标；
4. 用临时文件替换普通文件，创建符号链接；
5. 中途失败时按逆序回滚；macOS .app 重启前还会执行 codesign 校验；
6. 成功后删除临时更新目录并重启应用。

helper 从 stdin 接收完整脚本，并通过以下环境变量获得运行时路径：

~~~text
SIMPLE_UPDATER_PID
SIMPLE_UPDATER_INSTALL_ROOT
SIMPLE_UPDATER_PATCH_ROOT
SIMPLE_UPDATER_RESTART_PATH
~~~

更新目录和 helper 必须与当前安装目录分开。当前版本只支持 Windows 和 macOS 的实际更新执行；在 Linux 等其他系统上只能使用部分分析/清单 API，不能运行 updater shell。

## 10. 常见注意事项

- StartUpdater 成功后立即退出当前应用；否则目标程序可能继续占用待替换文件。
- ExecuteUpdate 成功时调用 os.Exit(0)，不要把它当作普通的“返回后继续执行”函数。
- Archive 与 PatchRoot/Script 互斥，不能混用。
- Compare 需要数据库中已有同一 system 和 appID 的版本；返回的 File.URL 才能供 DownloadPatch 下载。
- DownloadPatch 返回的是内存中的 []byte，大补丁会占用相应内存；当前 API 没有暴露写入 io.Writer 的流式版本。
- OSS AccessKey、PostgreSQL 密码应通过环境变量或密钥管理系统提供，不要写进源码。
- 仓库使用 Apache-2.0；DMG 读取器部分代码来源及许可见 THIRD_PARTY_NOTICES.md。

最小验证命令：

~~~bash
go test ./...
~~~
