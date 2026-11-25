import { useFocusEffect } from "expo-router";
import { useCallback } from "react";
import { AppState } from "react-native";

/**
 * Reloads when the screen comes into view (the first time, and on coming back from another screen)
 * and when the app comes back from the background, so old amounts do not stay up. Without the
 * spinner: nothing moves. A moment of `inactive` alone (a system sheet) is not a return.
 */
export function useReloadOnReturn(reload: (options: { spinner: false }) => Promise<void>) {
  useFocusEffect(
    useCallback(() => {
      void reload({ spinner: false });
      let away = false;
      const subscription = AppState.addEventListener("change", (next) => {
        if (next === "background") away = true;
        if (next === "active" && away) {
          away = false;
          void reload({ spinner: false });
        }
      });
      return () => subscription.remove();
    }, [reload]),
  );
}
