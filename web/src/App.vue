<script setup lang="ts">
import { RouterView } from 'vue-router'
import { CoToastHost } from '@/components/ui'
</script>

<template>
  <RouterView v-slot="{ Component }">
    <component :is="Component" />
  </RouterView>
  <CoToastHost />
</template>
