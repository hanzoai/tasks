// Tamagui config for Hanzo Tasks UI v2 spike.
//
// We start from @hanzogui/config (v5 default — same one the published
// `hanzogui` umbrella ships against) and override the dark palette so
// the page matches the v1 shadcn-style admin look at console.hanzo.ai.
//
// Tokens mirror ui/src/index.css (see HSL values in the comments).
// They are *approximated* in Tamagui's hex format — Tamagui doesn't
// take HSL strings directly in tokens, so we pre-compute the hex.
import { defaultConfig } from '@hanzogui/config/v5'
import { createHanzogui } from 'hanzogui'

// hsl(222, 47%, 5%)  → background
// hsl(222, 47%, 7%)  → card / popover
// hsl(222, 30%, 12%) → muted
// hsl(222, 30%, 15%) → secondary
// hsl(222, 30%, 18%) → accent / border / input
// hsl(0, 0%, 95%)    → foreground / primary
// hsl(215, 16%, 56%) → muted-foreground / ring
// hsl(0, 72%, 51%)   → destructive
const hanzoDark = {
  background: '#070b13',
  backgroundHover: '#0c121c',
  backgroundPress: '#0c121c',
  backgroundFocus: '#0c121c',
  backgroundStrong: '#0c121c',
  backgroundTransparent: 'rgba(7,11,19,0)',
  color: '#f2f2f2',
  colorHover: '#ffffff',
  colorPress: '#dcdcdc',
  colorFocus: '#ffffff',
  colorTransparent: 'rgba(242,242,242,0)',
  borderColor: '#222a39',
  borderColorHover: '#2c374a',
  borderColorPress: '#2c374a',
  borderColorFocus: '#3a4459',
  placeholderColor: '#7e8794',
  shadowColor: 'rgba(0,0,0,0.6)',
  shadowColorHover: 'rgba(0,0,0,0.7)',
  shadowColorPress: 'rgba(0,0,0,0.7)',
  shadowColorFocus: 'rgba(0,0,0,0.7)',
}

export const config = createHanzogui({
  ...defaultConfig,
  themes: {
    ...defaultConfig.themes,
    dark: { ...defaultConfig.themes.dark, ...hanzoDark },
    dark_hanzo: { ...defaultConfig.themes.dark, ...hanzoDark },
  },
})

export default config

export type Conf = typeof config

declare module 'hanzogui' {
  interface HanzoguiCustomConfig extends Conf {}
}
