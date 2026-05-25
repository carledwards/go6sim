//go:build !js

package netsim

// MaxHz reports the netsim transistor core's sustained ceiling on a
// native build. Measured at ~22 kHz on the development machine; well
// below interp because every half-step propagates state across the
// ~3500-transistor netlist.
func (a *Adapter) MaxHz() int { return 22000 }
