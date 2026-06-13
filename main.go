package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
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

func main() {
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

	sampleRate := uint32(16000)
	channels := uint16(1)
	format := malgo.FormatS16

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.DeviceID = devices[selectedIndex].ID.Pointer()
	deviceConfig.Capture.Format = format
	deviceConfig.Capture.Channels = uint32(channels)
	deviceConfig.SampleRate = sampleRate
	deviceConfig.PerformanceProfile = malgo.LowLatency

	var recordedSamples []byte
	var recordMu sync.Mutex
	isStopping := false // 📌 新增：标记是否正在进入结束阶段
	stopDone := make(chan struct{})

	onCapture := func(pOutputSample, pInputSample []byte, frameCount uint32) {
		if len(pInputSample) > 0 {
			recordMu.Lock()
			// 📌 即使按下回车，在主线程彻底调用 Stop 之前，依然允许缓冲数据写入，确保尾音完整
			if !isStopping {
				recordedSamples = append(recordedSamples, pInputSample...)
			}
			recordMu.Unlock()
		}
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
		fmt.Println("检测到回车，正在完美收尾最后音频（排空残留缓冲区）...")
	case <-sigChan:
		fmt.Println("检测到 Ctrl+C，正在完美收尾最后音频（排空残留缓冲区）...")
	}

	// 📌 核心修复 1：留出 350ms 时间让底层驱动把卡在队列里的最后几帧音频彻底回调给 onCapture
	time.Sleep(350 * time.Millisecond)

	// 📌 核心修复 2：此时缓冲区已完全排空，锁定状态停止接收后续可能产生的新杂音
	recordMu.Lock()
	isStopping = true
	recordMu.Unlock()

	// 📌 核心修复 3：最后安全地切断硬件设备
	if err := device.Stop(); err != nil {
		fmt.Printf("停止设备失败: %v\n", err)
	}

	fmt.Println("正在等待设备完全停止并处理最后音频...")
	select {
	case <-stopDone:
		fmt.Println("设备停止回调完成")
	case <-time.After(500 * time.Millisecond):
		fmt.Println("停止回调超时，继续后续处理")
	}

	fmt.Printf("录音已停止！共录制 %d 字节\n", len(recordedSamples))

	recordMu.Lock()
	recordedLen := len(recordedSamples)
	recordMu.Unlock()
	if recordedLen < 2 {
		fmt.Println("未录制到有效音频数据，程序退出")
		return
	}

	fmt.Println("\n=== 请选择要保存的音频格式 ===")
	fmt.Println("0 WAV 格式 (体积较大，无损，任何设备都能播)")
	fmt.Println("1 FLAC 格式 (体积小一半，无损，新版主流播放器通用)")
	fmt.Print("请输入选项 (0 或 1): ")

	text, _ := reader.ReadString('\n')
	text = strings.TrimSpace(text)
	formatChoice, err := strconv.Atoi(text)
	if err != nil {
		formatChoice = 0
	}

	recordMu.Lock()
	temp := make([]byte, len(recordedSamples))
	copy(temp, recordedSamples)
	recordMu.Unlock()

	numSamples := len(temp) / 2
	intBuffer := make([]int, numSamples)
	for i := 0; i < numSamples; i++ {
		raw := binary.LittleEndian.Uint16(temp[i*2 : i*2+2])
		val := int(int16(raw))
		intBuffer[i] = val
	}

	// 创建output目录
	err = os.MkdirAll("output", 0755)
	if err != nil {
		fmt.Printf("创建output目录失败: %v\n", err)
		return
	}

	// 生成带时间戳的文件名
	timestamp := time.Now().Format("2006-01-02_150405")

	switch formatChoice {
	case 1:
		outputFilename := fmt.Sprintf("output/output_%s.flac", timestamp)
		start := time.Now()
		err = saveAsFlacNew(outputFilename, intBuffer, sampleRate, channels)
		elapsed := time.Since(start)
		if err == nil {
			fmt.Printf("成功！已保存为新版无损压缩文件: %s\n", outputFilename)
			fmt.Printf("FLAC 转换耗时：%v\n", elapsed)
		}
	default:
		outputFilename := fmt.Sprintf("output/output_%s.wav", timestamp)
		audioBuf := &audio.IntBuffer{
			Format: &audio.Format{
				NumChannels: int(channels),
				SampleRate:  int(sampleRate),
			},
			Data:           intBuffer,
			SourceBitDepth: 16,
		}
		start := time.Now()
		err = saveAsWav(outputFilename, audioBuf)
		elapsed := time.Since(start)
		if err == nil {
			fmt.Printf("成功！已保存为标准无损文件: %s\n", outputFilename)
			fmt.Printf("WAV 转换耗时：%v\n", elapsed)
		}
	}

	if err != nil {
		fmt.Printf("文件保存失败: %v\n", err)
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
