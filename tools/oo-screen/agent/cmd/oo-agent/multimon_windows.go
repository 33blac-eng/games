//go:build windows

package main

import (
	"context"
	"log"
	"os"
	"os/exec"

	"github.com/organicoils/oo-screen/internal/multimon"
)

// startMultimonChildren — батько: по дитині на кожен монітор з
// multimonChildren. Діти живуть до ctx.Done() (ctx батька) або до смерті
// батька (EOF на їхньому stdin).
func startMultimonChildren(ctx context.Context, idxs []int, baseNode, logPath, token string) {
	if len(idxs) == 0 {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("oo-agent: multimon: os.Executable: %v — додаткові монітори не публікуються", err)
		return
	}
	env := multimonChildEnv(os.Environ(), token)
	for _, idx := range idxs {
		idx := idx
		args := multimonChildArgs(os.Args[1:], idx, baseNode, logPath, flagIsBool)
		log.Printf("oo-agent: multimon: монітор %d -> node %q", idx, multimon.NodeID(baseNode, idx))
		go superviseChild(ctx, idx, func() (func() error, error) {
			cmd := exec.CommandContext(ctx, exe, args...)
			cmd.Env = env
			// Труба stdin — «пуповина»: її закриває смерть батька. Тримаємо
			// посилання до Wait: інакше фіналізатор os.File закрив би її
			// раніше, і дитина вийшла б на ровному місці.
			umb, err := cmd.StdinPipe()
			if err != nil {
				return nil, err
			}
			if logPath == "" {
				cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			}
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return func() error {
				err := cmd.Wait()
				_ = umb.Close()
				return err
			}, nil
		})
	}
}
