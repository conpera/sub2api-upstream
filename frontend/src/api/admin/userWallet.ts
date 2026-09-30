import { apiClient } from '../client'

export interface UserWallet {
  paygo_balance: number
  monthly_balance: number
  monthly_held: number
  paygo_held: number
  total_available: number
  monthly_card: {id: number; granted: number; remaining: number; expires_at: string} | null
}
export interface WalletConsumption {
  id: number
  request_id: string
  monthly_cost: number
  paygo_cost: number
  created_at: string
}
export async function getUserWallet(userId: number): Promise<UserWallet> {
  const {data} = await apiClient.get<UserWallet>('/admin/users/' + userId + '/wallet')
  return data
}
export async function getUserWalletConsumption(userId: number, before = 0): Promise<WalletConsumption[]> {
  const {data} = await apiClient.get<WalletConsumption[]>('/admin/users/' + userId + '/wallet/consumption', {params:{before,limit:25}})
  return data
}
