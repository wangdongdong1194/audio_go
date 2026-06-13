# Go Audio Recorder (Malgo-based)

这是一个基于 `malgo` (MiniAudio Go 绑定) 开发的高性能、低延迟命令行录音工具。支持实时麦克风音频捕获，并能无损保存为 **WAV** 或 **FLAC** 压缩格式。

针对音频流多线程异步捕获的特点，本项目专门优化了缓冲区排空（Buffer Flush）机制，确保按下结束键时**绝不丢失最后 300ms 的尾音**。

## ✨ 核心特性

- 🎤 **设备自动枚举**：启动时自动扫描并列出系统中所有可用的麦克风输入设备。
- ⚡ **超低延迟捕获**：使用 `malgo.LowLatency` 性能配置，确保音频采集的实时性。
- 🎧 **双格式无损导出**：
  - **WAV**：标准无损格式，兼容性极佳。
  - **FLAC**：无损压缩格式，文件体积比 WAV 减小近一半。
- 🔒 **完美尾音保护**：内置状态标志位控制，按下回车后延迟安全关闭硬件，完美保留最后几帧残留音频。
- ⚙️ **标准音频采样**：默认采用语音识别最常用的 `16000Hz` 采样率、`单声道 (Mono)`、`16-bit` 位深。

## 🛠️ 环境依赖

由于 `malgo` 依赖底层 C 语言音频库（MiniAudio），在编译和运行本项目前，请确保系统具备以下 CGO 编译环境：

### 1. 安装 C 编译器
- **Windows**: 安装 [MinGW-w64](https://mingw-w64.org) 并将 `gcc` 加入系统环境变量。
- **macOS**: 安装 Xcode Command Line Tools (`xcode-select --install`)。
- **Linux**: 安装 `build-essential` 以及 ALSA 开发库（如 Ubuntu 下执行 `sudo apt-get install build-essential libasound2-dev`）。

### 2. 获取 Go 依赖包
```bash
go get github.com/gen2brain/malgo
go get github.com/go-audio/audio
go get github.com/go-audio/wav
go get github.com/mewkiz/flac
go get github.com/mewkiz/flac/frame
go get github.com/mewkiz/flac/meta

```

## 🚀 快速开始

### 运行程序
在项目根目录下直接运行：
```bash
go run main.go
```

### 使用步骤
1. **选择设备**：程序会打印麦克风列表，输入对应的编号（例如 `0`）并回车。
2. **开始录音**：看到 `正在录音... 请说话` 提示后即可开始说话。
3. **停止录音**：说话完毕后，**按下键盘回车键（Enter）** 或 `Ctrl+C`。程序会自动排空残留缓冲区并安全停止。
4. **选择格式**：
   - 输入 `0` 保存为 WAV 格式。
   - 输入 `1` 保存为 FLAC 格式。

### 文件输出
录音文件将自动创建并保存在本地的 `output/` 目录下，文件名带有当前时间戳，例如：
- `output/output_2026-06-14_001530.wav`
- `output/output_2026-06-14_001530.flac`

## 📂 项目结构说明

- `main()`: 负责初始化音频上下文、枚举并配置硬件设备，管理录音生命周期。
- `onCapture`: 核心音频数据回调函数，负责将硬件采集到的原始 PCM 字节流追加到内存缓冲区。
- `saveAsWav()`: 将 PCM 数据封装为标准 RIFF/WAV 格式文件。
- `saveAsFlacNew()`: 核心优化函数。将 16-bit 采样数据转换为 FLAC 帧（Frame），通过 `PredVerbatim` 模式实现快速无损压缩打包。

## 📝 开源协议

[MIT License](LICENSE)
