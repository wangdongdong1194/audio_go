package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

const (
	sampleRate = 16000
	channels   = 1
	bitDepth   = 16
	// 每 0.5 秒音频落地一次，控制内存占用
	flushInterval = 500 * time.Millisecond
)

func main() {
	// 目录初始化
	outDir := "output"
	tempDir := filepath.Join(outDir, ".temp")
	_ = os.MkdirAll(tempDir, 0755)

	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {})
	if err != nil {
		fmt.Printf("初始化音频上下文失败: %v\n", err)
		return
	}
	defer ctx.Uninit()

	devices, err := ctx.Devices(malgo.Capture)
	if err != nil {
		fmt.Printf("获取麦克风列表失败: %v\n", err)
		return
	}

	fmt.Println("=== 发现以下麦克风设备 ===")
	for i, device := range devices {
		fullInfo, err := ctx.DeviceInfo(malgo.Capture, device.ID, malgo.Shared)
		if err != nil {
			continue
		}
		fmt.Printf("[%d] 设备名称: %s\n", i, fullInfo.Name())
	}

	if len(devices) == 0 {
		fmt.Println("未检测到任何麦克风设备！")
		return
	}

	var selectedIndex int
	fmt.Printf("\n请选择麦克风编号 (0-%d): ", len(devices)-1)
	fmt.Scanf("%d", &selectedIndex)
	if selectedIndex < 0 || selectedIndex >= len(devices) {
		selectedIndex = 0
	}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.DeviceID = devices[selectedIndex].ID.Pointer()
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = channels
	deviceConfig.SampleRate = sampleRate
	deviceConfig.PerformanceProfile = malgo.LowLatency

	var memBuf []byte
	var recordMu sync.Mutex
	isStopping := false
	stopDone := make(chan struct{})
	segIdx := 0 // 分片序号

	// 定时落地协程
	flushTicker := time.NewTicker(flushInterval)
	defer flushTicker.Stop()
	flushStop := make(chan struct{})
	go func() {
		for {
			select {
			case <-flushTicker.C:
				recordMu.Lock()
				if len(memBuf) == 0 || isStopping {
					recordMu.Unlock()
					continue
				}
				// 拷贝当前缓存
				copyBuf := make([]byte, len(memBuf))
				copy(copyBuf, memBuf)
				memBuf = memBuf[:0] // 清空内存
				recordMu.Unlock()

				// 写入临时分片 raw PCM
				segFile := filepath.Join(tempDir, fmt.Sprintf("seg_%06d.raw", segIdx))
				f, err := os.Create(segFile)
				if err != nil {
					fmt.Printf("写入分片失败 %v\n", err)
					continue
				}
				_, _ = f.Write(copyBuf)
				f.Close()
				segIdx++
			case <-flushStop:
				return
			}
		}
	}()

	onCapture := func(pOutputSample, pInputSample []byte, frameCount uint32) {
		if len(pInputSample) == 0 {
			return
		}
		recordMu.Lock()
		if !isStopping {
			memBuf = append(memBuf, pInputSample...)
		}
		recordMu.Unlock()
	}

	captureCallbacks := malgo.DeviceCallbacks{
		Data: onCapture,
		Stop: func() {
			fmt.Println("设备停止回调已触发，录音会话结束")
			close(stopDone)
		},
	}
	device, err := malgo.InitDevice(ctx.Context, deviceConfig, captureCallbacks)
	if err != nil {
		fmt.Printf("打开麦克风失败: %v\n", err)
		return
	}
	defer device.Uninit()

	reader := bufio.NewReader(os.Stdin)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	_ = device.Start()
	fmt.Println("\n正在录音... 请说话（按回车或 Ctrl+C 停止录音）")

	stopChan := make(chan struct{})
	go func() {
		_, _ = reader.ReadString('\n')
		close(stopChan)
	}()

	select {
	case <-stopChan:
		fmt.Println("检测到回车，多录制 1.2 秒补全尾音...")
	case <-sigChan:
		fmt.Println("检测到 Ctrl+C，多录制 1.2 秒补全尾音...")
	}
	time.Sleep(1200 * time.Millisecond)

	// 标记停止，停止写入内存
	recordMu.Lock()
	isStopping = true
	recordMu.Unlock()
	close(flushStop) // 关闭定时落地协程

	// 把内存剩余缓存落地最后一片
	recordMu.Lock()
	if len(memBuf) > 0 {
		segFile := filepath.Join(tempDir, fmt.Sprintf("seg_%06d.raw", segIdx))
		f, _ := os.Create(segFile)
		_, _ = f.Write(memBuf)
		f.Close()
		segIdx++
		memBuf = memBuf[:0]
	}
	recordMu.Unlock()

	if err := device.Stop(); err != nil {
		fmt.Printf("停止设备失败: %v\n", err)
	}

	fmt.Println("等待硬件设备完全关闭...")
	select {
	case <-stopDone:
		fmt.Println("设备停止回调完成")
	case <-time.After(500 * time.Millisecond):
		fmt.Println("停止回调超时，继续合并分片")
	}

	// ===================== 读取所有分片合并完整PCM =====================
	files, err := filepath.Glob(filepath.Join(tempDir, "seg_*.raw"))
	if err != nil {
		fmt.Printf("读取临时分片失败: %v\n", err)
		return
	}
	if len(files) == 0 {
		fmt.Println("无录音分片，程序退出")
		return
	}
	// 按文件名序号升序排序
	sort.Strings(files)

	var fullPCM []byte
	for _, fp := range files {
		data, err := os.ReadFile(fp)
		if err != nil {
			fmt.Printf("读取分片 %s 失败 %v\n", fp, err)
			continue
		}
		fullPCM = append(fullPCM, data...)
	}
	fmt.Printf("录音已停止！合并后总音频字节: %d\n", len(fullPCM))
	if len(fullPCM) < 2 {
		fmt.Println("无有效音频数据")
		return
	}

	// 格式选择
	fmt.Println("\n=== 请选择要保存的音频格式 ===")
	fmt.Println("0 FLAC 格式 (体积比 WAV 小一半，无损，新版主流播放器通用)")
	fmt.Println("1 WAV 格式 (体积较大，无损，任何设备都能播)")
	fmt.Print("请输入选项 (0 或 1): ")

	text, _ := reader.ReadString('\n')
	text = strings.TrimSpace(text)
	formatChoice, err := strconv.Atoi(text)
	if err != nil {
		formatChoice = 0
	}

	// PCM字节转int样本
	numSamples := len(fullPCM) / 2
	intBuffer := make([]int, numSamples)
	for i := 0; i < numSamples; i++ {
		raw := binary.LittleEndian.Uint16(fullPCM[i*2 : i*2+2])
		val := int(int16(raw))
		intBuffer[i] = val
	}

	timestamp := time.Now().Format("2006-01-02_150405")
	var saveErr error
	switch formatChoice {
	case 1:
		outputFilename := filepath.Join(outDir, fmt.Sprintf("output_%s.wav", timestamp))
		audioBuf := &audio.IntBuffer{
			Format: &audio.Format{
				NumChannels: channels,
				SampleRate:  int(sampleRate),
			},
			Data:           intBuffer,
			SourceBitDepth: bitDepth,
		}
		start := time.Now()
		saveErr = saveAsWav(outputFilename, audioBuf)
		elapsed := time.Since(start)
		if saveErr == nil {
			fmt.Printf("成功保存: %s\n转换耗时：%v\n", outputFilename, elapsed)
		}
	default:
		outputFilename := filepath.Join(outDir, fmt.Sprintf("output_%s.flac", timestamp))
		start := time.Now()
		saveErr = saveAsFlacNew(outputFilename, intBuffer, sampleRate, channels)
		elapsed := time.Since(start)
		if saveErr == nil {
			fmt.Printf("成功保存: %s\n转换耗时：%v\n", outputFilename, elapsed)
		}
	}

	if saveErr != nil {
		fmt.Printf("文件保存失败: %v\n", saveErr)
	}
	// 询问是否清理临时分片
	fmt.Print("是否清理临时分片文件？(y/n): ")
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(strings.ToLower(choice))
	if choice == "y" || choice == "yes" {
		for _, f := range files {
			_ = os.Remove(f)
		}
		// 删除空的 temp 目录
		_ = os.Remove(tempDir)
		fmt.Println("已清理临时分片文件")
	} else {
		fmt.Println("保留临时分片文件")
	}
}

func saveAsWav(filename string, buf *audio.IntBuffer) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := wav.NewEncoder(file, buf.Format.SampleRate, 16, buf.Format.NumChannels, 1)
	if err := encoder.Write(buf); err != nil {
		return err
	}
	return encoder.Close()
}

func saveAsFlacNew(filename string, samples []int, sampleRate uint32, channels uint16) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	info := &meta.StreamInfo{
		BlockSizeMin:  16,
		BlockSizeMax:  65535,
		SampleRate:    sampleRate,
		NChannels:     uint8(channels),
		BitsPerSample: 16,
		NSamples:      uint64(len(samples)),
	}

	encoder, err := flac.NewEncoder(file, info)
	if err != nil {
		return err
	}
	defer encoder.Close()

	const blockSize = 4096
	totalSamples := len(samples)

	for i := 0; i < totalSamples; i += blockSize {
		end := i + blockSize
		if end > totalSamples {
			end = totalSamples
		}
		chunk := samples[i:end]
		blockLen := len(chunk)

		subframes := make([]*frame.Subframe, channels)
		sample32 := make([]int32, blockLen)

		for idx, v := range chunk {
			if v < -32768 {
				v = -32768
			} else if v > 32767 {
				v = 32767
			}
			sample32[idx] = int32(v)
		}

		subframes[0] = &frame.Subframe{
			Samples:  sample32,
			NSamples: blockLen,
			SubHeader: frame.SubHeader{
				Pred: frame.PredVerbatim,
			},
		}

		var chMode frame.Channels
		switch channels {
		case 1:
			chMode = frame.ChannelsMono
		case 2:
			chMode = frame.ChannelsLR
		default:
			chMode = frame.ChannelsMono
		}

		f := &frame.Frame{
			Header: frame.Header{
				BlockSize:     uint16(blockLen),
				SampleRate:    sampleRate,
				Channels:      chMode,
				BitsPerSample: 16,
			},
			Subframes: subframes,
		}

		if err := encoder.WriteFrame(f); err != nil {
			return err
		}
	}

	return nil
}