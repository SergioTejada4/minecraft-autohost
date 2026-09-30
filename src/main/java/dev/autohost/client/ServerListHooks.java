package dev.autohost.client;

import dev.autohost.AutoHostMod;
import net.minecraft.client.gui.components.Button;
import net.minecraft.client.gui.screens.multiplayer.JoinMultiplayerScreen;
import net.minecraft.client.gui.screens.multiplayer.ServerSelectionList;
import net.minecraft.client.multiplayer.ServerData;
import net.minecraft.network.chat.Component;
import net.neoforged.api.distmarker.Dist;
import net.neoforged.bus.api.SubscribeEvent;
import net.neoforged.fml.common.EventBusSubscriber;
import net.neoforged.neoforge.client.event.ScreenEvent;

@EventBusSubscriber(modid = AutoHostMod.MOD_ID, value = Dist.CLIENT)
public final class ServerListHooks {
    private static JoinMultiplayerScreen lastClickScreen;
    private static double lastClickX;
    private static double lastClickY;
    private static long lastClickAt;

    private ServerListHooks() {
    }

    @SubscribeEvent
    public static void onServerListMouse(ScreenEvent.MouseButtonPressed.Pre event) {
        if (!(event.getScreen() instanceof JoinMultiplayerScreen screen) || event.getButton() != 0) return;
        ServerSelectionList list = selectionList(screen);
        ServerData iconJoinTarget = iconJoinTarget(list, event.getMouseX(), event.getMouseY());
        if (iconJoinTarget != null) {
            if (AutoHostCoordinator.interceptJoin(screen, iconJoinTarget)) event.setCanceled(true);
            return;
        }
        boolean doubleClick = isServerRowDoubleClick(screen, list, event.getMouseX(), event.getMouseY());
        ServerData selected = selectedServer(list);
        if (selected == null) return;

        boolean joinButton = isJoinButtonClicked(screen, event.getMouseX(), event.getMouseY());
        if ((joinButton || doubleClick) && AutoHostCoordinator.interceptJoin(screen, selected)) {
            event.setCanceled(true);
        }
    }

    @SubscribeEvent
    public static void onServerListKey(ScreenEvent.KeyPressed.Pre event) {
        if (!(event.getScreen() instanceof JoinMultiplayerScreen screen)) return;
        if (event.getKeyCode() != 257 && event.getKeyCode() != 335) return;
        ServerData selected = selectedServer(selectionList(screen));
        if (selected != null && AutoHostCoordinator.interceptJoin(screen, selected)) {
            event.setCanceled(true);
        }
    }

    private static ServerSelectionList selectionList(JoinMultiplayerScreen screen) {
        for (var child : screen.children()) {
            if (child instanceof ServerSelectionList list) return list;
        }
        return null;
    }

    private static ServerData selectedServer(ServerSelectionList list) {
        if (list == null || !(list.getSelected() instanceof ServerSelectionList.OnlineServerEntry entry)) return null;
        return entry.getServerData();
    }

    private static ServerData iconJoinTarget(ServerSelectionList list, double mouseX, double mouseY) {
        if (list == null || mouseX <= list.getRowLeft() + 16 || mouseX >= list.getRowLeft() + 32
                || mouseY < list.getY() || mouseY >= list.getY() + list.getHeight()) {
            return null;
        }
        int rowOffset = (int) Math.floor(mouseY - list.getY()) + (int) list.getScrollAmount() - 4;
        if (rowOffset < 0) return null;
        int rowIndex = rowOffset / 36;
        if (rowIndex < 0 || rowIndex >= list.children().size()
                || !(list.children().get(rowIndex) instanceof ServerSelectionList.OnlineServerEntry entry)) {
            return null;
        }
        return entry.getServerData();
    }

    private static boolean isJoinButtonClicked(JoinMultiplayerScreen screen, double mouseX, double mouseY) {
        String joinLabel = Component.translatable("selectServer.select").getString();
        for (var child : screen.children()) {
            if (child instanceof Button button && button.active && button.getMessage().getString().equals(joinLabel)
                    && mouseX >= button.getX() && mouseX < button.getX() + button.getWidth()
                    && mouseY >= button.getY() && mouseY < button.getY() + button.getHeight()) {
                return true;
            }
        }
        return false;
    }

    private static boolean isServerRowDoubleClick(JoinMultiplayerScreen screen, ServerSelectionList list,
                                                  double mouseX, double mouseY) {
        if (list == null || mouseX < list.getX() || mouseX >= list.getX() + list.getWidth()
                || mouseY < list.getY() || mouseY >= list.getY() + list.getHeight()) {
            lastClickScreen = null;
            return false;
        }
        long now = System.currentTimeMillis();
        boolean doubleClick = lastClickScreen == screen && now - lastClickAt <= 300
                && Math.abs(mouseX - lastClickX) <= 32 && Math.abs(mouseY - lastClickY) <= 36;
        lastClickScreen = screen;
        lastClickX = mouseX;
        lastClickY = mouseY;
        lastClickAt = now;
        return doubleClick;
    }
}