package ticket

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"billgo/internal/blob"
	"billgo/internal/envelope"
)

// runJob 是接活侧的执行体：把表单交给 Agent 进程，收集输出，交付或拒绝。
func (s *Service) runJob(ticketID string, hop int, from string, form json.RawMessage) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	jobDir := filepath.Join(s.cfg.DataDir, "jobs", fmt.Sprintf("%s-%d", ticketID, hop))
	outDir := filepath.Join(jobDir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		s.finishRejected(ticketID, hop, from, "建作业目录失败: "+err.Error())
		return
	}
	jobFile := map[string]any{"ticket": ticketID, "hop": hop, "from": from, "form": form}
	raw, _ := json.MarshalIndent(jobFile, "", "  ")
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), raw, 0o644); err != nil {
		s.finishRejected(ticketID, hop, from, "写 job.json 失败: "+err.Error())
		return
	}

	if len(s.cfg.AgentCmd) == 0 {
		s.finishRejected(ticketID, hop, from, "节点没有配置 agent_cmd")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.cfg.JobTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cfg.AgentCmd[0], s.cfg.AgentCmd[1:]...)
	cmd.Dir = jobDir
	cmd.Env = append(os.Environ(),
		"BILLGO_TICKET="+ticketID,
		fmt.Sprintf("BILLGO_HOP=%d", hop),
		"BILLGO_FROM="+from,
		"BILLGO_JOB_DIR="+jobDir,
		"BILLGO_OUT_DIR="+outDir,
		"BILLGO_DATA_DIR="+s.cfg.DataDir,
	)
	logFile, err := os.Create(filepath.Join(jobDir, "agent.log"))
	if err != nil {
		s.finishRejected(ticketID, hop, from, "建日志失败: "+err.Error())
		return
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	runErr := cmd.Run()
	logFile.Close()

	if ctx.Err() == context.DeadlineExceeded {
		s.finishRejected(ticketID, hop, from, "作业超时")
		return
	}
	if runErr != nil {
		reason := readRejectReason(jobDir)
		if reason == "" {
			reason = "执行失败: " + runErr.Error()
		}
		s.finishRejected(ticketID, hop, from, reason)
		return
	}

	payload, err := packOut(outDir)
	if err != nil {
		s.finishRejected(ticketID, hop, from, "打包交付物失败: "+err.Error())
		return
	}
	hash, size, err := blob.PutBytes(s.blobDir, payload)
	if err != nil {
		s.finishRejected(ticketID, hop, from, "落盘交付物失败: "+err.Error())
		return
	}
	receipt := Receipt{
		Ticket: ticketID, Hop: hop, Blob: hash, Size: size,
		From: s.id.PeerID, To: from, TS: time.Now().UnixMilli(),
	}
	if err := SignReceipt(s.id.Private, &receipt); err != nil {
		slog.Error("签回执失败", "ticket", ticketID, "err", err)
		return
	}
	env, err := envelope.New(envelope.TypeHopDeliver, from, ticketID, hop, time.Now().UnixMilli(),
		DeliverBody{Blob: hash, Size: size, Receipt: receipt})
	if err != nil {
		slog.Error("构造交付信封失败", "ticket", ticketID, "err", err)
		return
	}
	if err := env.Sign(s.id.Private); err != nil {
		slog.Error("签交付信封失败", "ticket", ticketID, "err", err)
		return
	}
	if err := s.out.Enqueue(from, env); err != nil {
		slog.Error("交付入队失败", "ticket", ticketID, "err", err)
		return
	}
	_ = s.saveJob(JobRecord{Ticket: ticketID, Hop: hop, From: from, Status: "delivered"})
	slog.Info("作业完成并交付", "ticket", ticketID, "hop", hop, "blob", hash, "size", size)
}

func (s *Service) finishRejected(ticketID string, hop int, from, reason string) {
	slog.Warn("作业被拒绝", "ticket", ticketID, "hop", hop, "reason", reason)
	env, err := envelope.New(envelope.TypeHopReject, from, ticketID, hop, time.Now().UnixMilli(),
		RejectBody{Reason: reason})
	if err != nil {
		return
	}
	if err := env.Sign(s.id.Private); err != nil {
		return
	}
	if err := s.out.Enqueue(from, env); err != nil {
		slog.Error("拒绝入队失败", "ticket", ticketID, "err", err)
		return
	}
	_ = s.saveJob(JobRecord{Ticket: ticketID, Hop: hop, From: from, Status: "rejected", Err: reason})
}

// readRejectReason 读 Agent 写的 result.json 里的拒绝原因。
func readRejectReason(jobDir string) string {
	raw, err := os.ReadFile(filepath.Join(jobDir, "result.json"))
	if err != nil {
		return ""
	}
	var r struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return ""
	}
	return r.Reason
}

// packOut 把 out/ 打成一份交付物。只有一个顶层文件时直接用它，
// 否则打 tar.gz，保证同样的内容得到同样的哈希。
func packOut(outDir string) ([]byte, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, err
	}
	if len(entries) == 1 && !entries[0].IsDir() {
		return os.ReadFile(filepath.Join(outDir, entries[0].Name()))
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(outDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == outDir {
			return nil
		}
		rel, err := filepath.Rel(outDir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{
				Name: rel + "/", Mode: 0o755, Typeflag: tar.TypeDir,
				ModTime: time.Unix(0, 0),
			})
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: rel, Mode: int64(info.Mode().Perm()), Size: info.Size(),
			Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0),
		}); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
