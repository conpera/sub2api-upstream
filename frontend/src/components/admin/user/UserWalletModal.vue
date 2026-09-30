<template>
  <BaseDialog :show="show" :title="t('admin.users.twoBalance.title')" width="wide" @close="$emit('close')">
    <p class="mb-4 text-sm text-gray-500">{{ user?.email }}</p>
    <p v-if="loading" role="status">{{ t('common.loading') }}</p>
    <div v-else-if="error" role="alert" class="space-y-3">
      <p>{{ error }}</p><button class="btn btn-secondary" @click="load">{{ t('admin.users.twoBalance.refresh') }}</button>
    </div>
    <div v-else-if="wallet" class="space-y-5">
      <div class="grid gap-3 sm:grid-cols-2">
        <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
          <p class="text-sm text-gray-500">{{ t('admin.users.twoBalance.monthly') }}</p>
          <p class="mt-1 text-xl font-semibold tabular-nums">{{ money(wallet.monthly_balance - wallet.monthly_held) }}</p>
          <p class="mt-2 text-xs text-gray-500">{{ wallet.monthly_card ? t('admin.users.twoBalance.expires', {date: new Date(wallet.monthly_card.expires_at).toLocaleString()}) : t('admin.users.twoBalance.noCard') }}</p>
        </div>
        <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
          <p class="text-sm text-gray-500">PAYGO</p>
          <p class="mt-1 text-xl font-semibold tabular-nums">{{ money(wallet.paygo_balance - wallet.paygo_held) }}</p>
          <p class="mt-2 text-xs text-gray-500">{{ t('admin.users.twoBalance.permanent') }}</p>
        </div>
      </div>
      <p class="text-sm">{{ t('admin.users.twoBalance.total') }}: <strong>{{ money(wallet.total_available) }}</strong></p>
      <p v-if="wallet.monthly_held + wallet.paygo_held > 0" class="text-sm text-gray-500">{{ t('admin.users.twoBalance.reserved', {amount: money(wallet.monthly_held + wallet.paygo_held)}) }}</p>
      <div class="overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead><tr><th class="py-2">{{ t('admin.users.twoBalance.request') }}</th><th class="py-2">{{ t('admin.users.twoBalance.monthly') }}</th><th class="py-2">PAYGO</th></tr></thead>
          <tbody>
            <tr v-for="row in consumption" :key="row.id" class="border-t border-gray-100 dark:border-dark-600">
              <td class="max-w-[240px] break-all py-2"><p>{{ row.request_id }}</p><time class="text-xs text-gray-500">{{ new Date(row.created_at).toLocaleString() }}</time></td>
              <td class="px-2 py-2 tabular-nums">{{ money(row.monthly_cost) }}</td><td class="px-2 py-2 tabular-nums">{{ money(row.paygo_cost) }}</td>
            </tr>
            <tr v-if="!consumption.length"><td colspan="3" class="py-4 text-gray-500">{{ t('admin.users.twoBalance.empty') }}</td></tr>
          </tbody>
        </table>
      </div>
      <button v-if="hasMore" :disabled="loadingMore" class="btn btn-secondary" @click="more">{{ t('admin.users.twoBalance.more') }}</button>
    </div>
    <template #footer><button class="btn btn-secondary" @click="$emit('close')">{{ t('common.close') }}</button></template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AdminUser } from '@/types'
import { getUserWallet, getUserWalletConsumption, type UserWallet, type WalletConsumption } from '@/api/admin/userWallet'
import { extractApiErrorMessage } from '@/utils/apiError'
import BaseDialog from '@/components/common/BaseDialog.vue'
const props = defineProps<{show:boolean; user:AdminUser|null}>()
defineEmits(['close'])
const {t} = useI18n()
const wallet = ref<UserWallet|null>(null)
const consumption = ref<WalletConsumption[]>([])
const loading = ref(false), loadingMore = ref(false), error = ref(''), hasMore = ref(false)
let generation = 0
const money = (value:number) => '$' + value.toLocaleString(undefined,{minimumFractionDigits:2,maximumFractionDigits:8})
async function load() {
  const id=props.user?.id, current=++generation
  if (!id || !props.show) return
  loading.value=true; error.value=''; consumption.value=[]; wallet.value=null
  try {
    const [balance,rows] = await Promise.all([getUserWallet(id),getUserWalletConsumption(id)])
    if (current !== generation) return
    wallet.value=balance; consumption.value=rows; hasMore.value=rows.length===25
  } catch (e) { if(current===generation) error.value=extractApiErrorMessage(e,t('common.error')) }
  finally { if(current===generation) loading.value=false }
}
async function more() {
  const id=props.user?.id, current=generation
  if(!id || loadingMore.value) return
  loadingMore.value=true
  try {
    const rows=await getUserWalletConsumption(id,consumption.value.at(-1)?.id ?? 0)
    if(current===generation) { consumption.value.push(...rows); hasMore.value=rows.length===25 }
  } catch(e) { if(current===generation) error.value=extractApiErrorMessage(e,t('common.error')) }
  finally { loadingMore.value=false }
}
watch(() => [props.show,props.user?.id], () => { if(props.show) void load(); else generation++ },{immediate:true})
</script>
