package dev.autohost.client;

import dev.autohost.AutoHostMod;
import net.minecraft.client.Minecraft;
import net.neoforged.api.distmarker.Dist;
import net.neoforged.bus.api.SubscribeEvent;
import net.neoforged.fml.common.EventBusSubscriber;
import net.neoforged.neoforge.client.event.ClientTickEvent;

@EventBusSubscriber(modid = AutoHostMod.MOD_ID, value = Dist.CLIENT)
public final class ClientHooks {
    private ClientHooks() {
    }

    @SubscribeEvent
    public static void onClientTick(ClientTickEvent.Post event) {
        AutoHostCoordinator.onClientTick(Minecraft.getInstance());
    }
}