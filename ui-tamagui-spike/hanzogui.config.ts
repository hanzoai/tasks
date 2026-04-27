// Tamagui config for Hanzo Tasks UI v2.
//
// Use the canonical @hanzogui/config v5 default (same shape the
// starter at ~/work/hanzo/gui/code/starters/expo-router uses) so
// Tamagui's runtime theme registry resolves cleanly. Hanzo brand
// recoloring lives in src/index.css as CSS variables that we layer
// on top of Tamagui's `dark` theme, NOT by mutating the themes
// object — spreading it through a JS module breaks the static
// shape Tamagui's getThemeProxied() depends on.

import { defaultConfig } from '@hanzogui/config/v5'
import { createHanzogui } from 'hanzogui'

export const config = createHanzogui(defaultConfig)

export default config

export type Conf = typeof config

declare module 'hanzogui' {
  interface HanzoguiCustomConfig extends Conf {}
}
