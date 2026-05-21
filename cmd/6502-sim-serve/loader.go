package main

import (
	"context"
	"fmt"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/machine"
)

// presetLoader implements bridge.Loader by wiring the machine package's
// concrete presets in. This is the *only* place go6sim's runtime
// process imports go6sim/machine into the bridge surface — keeps the
// bridge package itself dep-free of any preset (the carve invariant).
type presetLoader struct{}

func (presetLoader) Presets() []bridge.PresetInfo {
	return []bridge.PresetInfo{
		{Name: "teach-min", Label: "Teach Minimal", Summary: "RAM + framebuffer RAM + 6522 VIA + ROM"},
		{Name: "teach-merlin", Label: "Teach Merlin", Summary: "RAM + two 6522 VIAs + ROM (multi-VIA)"},
		{Name: "vic-demo", Label: "VIC Demo", Summary: "Standalone VIC display demo (color/char planes + controller + VIA)"},
	}
}

func (presetLoader) Load(name string, image []byte) (*bridge.Hub, func(), error) {
	var m *machine.Machine
	var regs []bridge.Region

	switch name {
	case "teach-min":
		m = machine.TeachMin()
		regs = []bridge.Region{
			{Name: "ram", Lo: 0x0000, Hi: 0x1FFF, ReadOnly: false},
			{Name: "framebuffer", Lo: 0xA000, Hi: 0xAFFF, ReadOnly: false},
			{Name: "via1", Lo: 0xB000, Hi: 0xB00F, ReadOnly: false},
			{Name: "rom", Lo: 0xE000, Hi: 0xFFFF, ReadOnly: true},
		}
	case "teach-merlin":
		m = machine.TeachMerlin()
		regs = []bridge.Region{
			{Name: "ram", Lo: 0x0000, Hi: 0x1FFF, ReadOnly: false},
			{Name: "via1", Lo: 0xB000, Hi: 0xB00F, ReadOnly: false},
			{Name: "via2", Lo: 0xB100, Hi: 0xB10F, ReadOnly: false},
			{Name: "rom", Lo: 0xE000, Hi: 0xFFFF, ReadOnly: true},
		}
	case "vic-demo":
		m = machine.VICDemo()
		regs = []bridge.Region{
			{Name: "ram", Lo: 0x0000, Hi: 0x1FFF, ReadOnly: false},
			{Name: "framebuffer", Lo: 0xA000, Hi: 0xA7FF, ReadOnly: false},
			{Name: "via1", Lo: 0xB000, Hi: 0xB00F, ReadOnly: false},
			{Name: "rom", Lo: 0xE000, Hi: 0xFFFF, ReadOnly: true},
		}
	default:
		return nil, nil, fmt.Errorf("unknown preset %q", name)
	}

	if len(image) > 0 {
		if err := m.Load(image); err != nil {
			return nil, nil, fmt.Errorf("loading image: %w", err)
		}
	}
	// Per-session Hub: each bridge connection gets its own Pump.
	// cleanup cancels it on session teardown.
	hub := bridge.NewHub(m.Inst, regs, name)
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	cleanup := func() {
		cancel()
		<-hub.Done()
	}
	return hub, cleanup, nil
}
