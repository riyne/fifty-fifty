import { useColorScheme } from "react-native";

// One calm palette: neutrals, and one muted green used for actions and the claim mark only.
// Money owed by you is rust rather than red, so red is left for errors and destructive actions.

const light = {
  background: "#F3F5F4",
  card: "#FFFFFF",
  pressed: "#ECEFED",
  text: "#17201C",
  secondary: "#5B6761",
  hairline: "#E0E5E2",
  accent: "#1F6B4F",
  onAccent: "#FFFFFF",
  accentSoft: "#E2EEE8",
  owe: "#98481F",
  oweSoft: "#F6E9E0",
  danger: "#B3261E",
  dangerSoft: "#FAE6E4",
};

export type Colors = typeof light;

const dark: Colors = {
  background: "#0F1311",
  card: "#1A201D",
  pressed: "#252D29",
  text: "#ECF1EE",
  secondary: "#9DAAA3",
  hairline: "#2D3632",
  accent: "#62BD95",
  onAccent: "#0A1F16",
  accentSoft: "#1D352B",
  owe: "#E29C70",
  oweSoft: "#38271C",
  danger: "#F28B82",
  dangerSoft: "#3B211F",
};

export function useColors(): Colors {
  return useColorScheme() === "dark" ? dark : light;
}

export const space = { xs: 4, s: 8, m: 12, l: 16, xl: 24, xxl: 40 } as const;
export const radius = { s: 10, m: 16 } as const;
